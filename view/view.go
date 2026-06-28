// Package view renders HTML pages with html/template over a shared layout, and
// can serve them with ETag/304 and gzip from an in-memory output cache.
//
// Go's template engine has a flat namespace: if two pages define a block with
// the same name (e.g. "content"), they collide. To avoid it, each page is
// compiled into its own set, cloned from a base holding the layout and shared
// partials. So every page can reuse the same block names without clashing.
//
// File convention inside the fs.FS:
//
//	layout.html        → shared skeleton; declares {{block "head" .}} and {{block "content" .}}
//	partial/*.html     → reusable fragments ({{define "name"}}...{{end}})
//	*.html             → one page per file; defines "head" and "content"
//
// The source is an fs.FS, so the same code serves templates from a mounted
// volume (os.DirFS) or baked into the binary (embed.FS) — the caller chooses:
//
//	e, err := view.New(os.DirFS("web"))            // files on disk / volume
//	e, err := view.New(embedded)                   // baked into the binary
//	e, err := view.New(os.DirFS("web"), view.WithReload(developing)) // dev
//
// Rendering with a cache key stores the rendered output (plus its ETag and gzip
// form) so repeat requests are a map lookup and a conditional 304, skipping both
// the render and the body transfer. Without a key the page is rendered every
// time and served uncompressed (use a cache key, or a compression middleware,
// where it matters). Zero dependencies — standard library only.
package view

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

const (
	layout      = "layout.html"
	contentType = "text/html; charset=utf-8"
	gzipMinSize = 1024
)

// buffers recycles the bytes.Buffer used to render. Each page is assembled in
// memory first (so a template failing mid-execution never emits partial HTML),
// and reusing the buffer avoids allocating tens of KB of garbage per render.
var buffers = sync.Pool{New: func() any {
	b := new(bytes.Buffer)
	b.Grow(48 << 10) // typical page size; skips the initial regrow
	return b
}}

// maxBufferRetenido caps the capacity returned to the pool: an unusually large
// render must not leave a giant buffer pinned forever.
const maxBufferRetenido = 256 << 10

// entrada is a cached rendered page: body, its gzip form (nil when gzip doesn't
// pay off) and a strong ETag over the body. bodyLen/gzipLen hold the
// Content-Length as a precomputed string so the hot path avoids strconv.Itoa per
// request (the length is fixed once the entry is built).
type entrada struct {
	body    []byte
	gzip    []byte
	etag    string
	bodyLen string
	gzipLen string // "" when gzip == nil
}

// Engine compiles and renders the pages of an fs.FS.
type Engine struct {
	fsys   fs.FS
	funcs  template.FuncMap
	reload bool

	mu    sync.RWMutex
	pages map[string]*template.Template

	cmu   sync.RWMutex
	cache map[string]*entrada
}

// Option configures an Engine at construction time.
type Option func(*Engine)

// WithFuncs registers template functions available to every page.
func WithFuncs(funcs template.FuncMap) Option {
	return func(e *Engine) { e.funcs = funcs }
}

// WithReload, when enabled, recompiles the templates on every render and
// bypasses the output cache, so edits show up without a restart. Meant for
// development; leave it off in production.
func WithReload(enabled bool) Option {
	return func(e *Engine) { e.reload = enabled }
}

// New compiles the templates in fsys.
func New(fsys fs.FS, opts ...Option) (*Engine, error) {
	e := &Engine{fsys: fsys, cache: make(map[string]*entrada)}
	for _, opt := range opts {
		opt(e)
	}
	if err := e.compilar(); err != nil {
		return nil, err
	}
	return e, nil
}

// RenderOption configures a single Render call.
type RenderOption func(*renderOpts)

type renderOpts struct{ cacheKey string }

// WithCache caches this page's rendered output (with ETag and gzip) under key,
// so later renders with the same key are served from memory with a 304 when the
// client's ETag still matches. Keys must come from a bounded set (page name, or
// page + a small set of variants); an unbounded key space grows the cache
// without bound.
func WithCache(key string) RenderOption {
	return func(o *renderOpts) { o.cacheKey = key }
}

// Render writes the page into w, executing it inside the layout, and sets the
// usual headers (Content-Type, Cache-Control: no-cache). With WithCache it adds
// an ETag, answers conditional requests with 304, and negotiates gzip; the
// output is rendered once and cached. Without it the page is rendered every
// call and served uncompressed.
//
// On a render error nothing is written to the body (the page is assembled in a
// buffer first), so the caller can still send its own error response.
func (e *Engine) Render(w http.ResponseWriter, r *http.Request, page string, data any, opts ...RenderOption) error {
	var ro renderOpts
	for _, opt := range opts {
		opt(&ro)
	}

	if e.reload {
		if err := e.compilar(); err != nil {
			return err
		}
	}

	// Camino cacheado (solo en producción): hit servido desde memoria, miss
	// renderiza una vez, guarda y sirve.
	if ro.cacheKey != "" && !e.reload {
		if ent := e.cacheGet(ro.cacheKey); ent != nil {
			servir(w, r, ent)
			return nil
		}
		ent, err := e.construir(page, data)
		if err != nil {
			return err
		}
		e.cachePut(ro.cacheKey, ent)
		servir(w, r, ent)
		return nil
	}

	// Sin cache (o reload en dev): stream directo, sin ETag ni gzip.
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-cache")
	return e.stream(w, page, data)
}

// compilar builds the base (layout + partials) and, on a clone of it, each page
// file at the fs.FS root.
func (e *Engine) compilar() error {
	base := template.New(layout).Funcs(e.funcs)
	if _, err := base.ParseFS(e.fsys, layout); err != nil {
		return err
	}
	if parciales, _ := fs.Glob(e.fsys, "partial/*.html"); len(parciales) > 0 {
		if _, err := base.ParseFS(e.fsys, "partial/*.html"); err != nil {
			return err
		}
	}

	archivos, err := fs.Glob(e.fsys, "*.html")
	if err != nil {
		return err
	}
	pages := make(map[string]*template.Template, len(archivos))
	for _, archivo := range archivos {
		nombre := strings.TrimSuffix(path.Base(archivo), ".html")
		if nombre == strings.TrimSuffix(layout, ".html") {
			continue
		}
		set, err := base.Clone()
		if err != nil {
			return err
		}
		if _, err := set.ParseFS(e.fsys, archivo); err != nil {
			return err
		}
		pages[nombre] = set
	}

	e.mu.Lock()
	e.pages = pages
	e.mu.Unlock()
	e.cmu.Lock()
	e.cache = make(map[string]*entrada)
	e.cmu.Unlock()
	return nil
}

func (e *Engine) lookup(page string) (*template.Template, error) {
	e.mu.RLock()
	set, ok := e.pages[page]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("view: página %q no encontrada", page)
	}
	return set, nil
}

// stream renders page directly into w, recycling a pooled buffer. No copy of the
// output is kept.
func (e *Engine) stream(w io.Writer, page string, data any) error {
	set, err := e.lookup(page)
	if err != nil {
		return err
	}
	buf := buffers.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxBufferRetenido {
			buffers.Put(buf)
		}
	}()
	if err := set.ExecuteTemplate(buf, layout, data); err != nil {
		return err
	}
	_, err = buf.WriteTo(w)
	return err
}

// construir renders page and builds its cache entry (copied body, ETag, gzip).
func (e *Engine) construir(page string, data any) (*entrada, error) {
	set, err := e.lookup(page)
	if err != nil {
		return nil, err
	}
	buf := buffers.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxBufferRetenido {
			buffers.Put(buf)
		}
	}()
	if err := set.ExecuteTemplate(buf, layout, data); err != nil {
		return nil, err
	}
	body := bytes.Clone(buf.Bytes())
	ent := &entrada{body: body, etag: makeETag(body), gzip: compress(body)}
	ent.bodyLen = strconv.Itoa(len(ent.body))
	if ent.gzip != nil {
		ent.gzipLen = strconv.Itoa(len(ent.gzip))
	}
	return ent, nil
}

func (e *Engine) cacheGet(key string) *entrada {
	e.cmu.RLock()
	defer e.cmu.RUnlock()
	return e.cache[key]
}

func (e *Engine) cachePut(key string, ent *entrada) {
	e.cmu.Lock()
	e.cache[key] = ent
	e.cmu.Unlock()
}

// servir writes a cached entry: headers, 304 when the ETag matches, otherwise
// the (optionally gzip-encoded) body.
func servir(w http.ResponseWriter, r *http.Request, ent *entrada) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("ETag", ent.etag)
	h.Set("Cache-Control", "no-cache")
	h.Set("Vary", "Accept-Encoding")

	if noneMatch(r.Header.Get("If-None-Match"), ent.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := ent.body
	length := ent.bodyLen
	if ent.gzip != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = ent.gzip
		length = ent.gzipLen
	}
	h.Set("Content-Length", length)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// makeETag returns a strong ETag over b. The same ETag covers the raw and gzip
// forms; Vary: Accept-Encoding tells caches the representation depends on the
// request.
func makeETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// compress returns the gzip form of b, or nil if it isn't worth it.
func compress(b []byte) []byte {
	if len(b) < gzipMinSize {
		return nil
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write(b); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	if buf.Len() >= len(b) {
		return nil
	}
	return bytes.Clone(buf.Bytes())
}

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// noneMatch reports whether the client's If-None-Match header covers etag.
func noneMatch(header, etag string) bool {
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	return strings.Contains(header, etag)
}
