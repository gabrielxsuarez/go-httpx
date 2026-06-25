// Package static serves static files from memory.
//
// It reads an fs.FS once at startup and precomputes, per file, its content
// type, ETag and gzip-compressed form. Serving is then a map lookup plus header
// negotiation: no filesystem access and no per-request compression on the hot
// path. Conditional requests (If-None-Match) are answered with 304.
//
// The source of the files is chosen by the caller through the fs.FS, so the
// same code serves them whether they are baked into the binary or read from
// disk:
//
//	srv, err := static.New(os.DirFS("/app/static"))  // files live on a mounted volume
//	srv, err := static.New(embedded)                 // files baked into the binary
//
//	mux.Handle("GET /", srv)
//
// In either case the files are read once, at New, and served from memory. With
// os.DirFS the directory is the source of truth (e.g. a host volume bind-mounted
// into a container): nothing is embedded, and changes are picked up on restart.
//
// For development, WithReload(true) reads and rebuilds each asset from the fs.FS
// on every request, so edits are reflected without restarting:
//
//	srv, err := static.New(os.DirFS("static"), static.WithReload(developing))
//
// The surface is deliberately small because this package is shared across
// projects. New behaviour is added through Option values, not new types.
package static

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// gzipMinSize is the smallest file worth compressing. Below it the gzip framing
// and the second copy in memory don't pay off.
const gzipMinSize = 1024

// asset is a single file resolved at startup. gzip is nil when compression
// doesn't shrink the file (already-compressed formats, tiny files), so the
// server never holds a useless second copy.
type asset struct {
	contentType string
	etag        string
	raw         []byte
	gzip        []byte
}

// Server serves a set of assets. With reload off it is read-only after New
// (precomputed map, safe for concurrent use without locks); with reload on it
// reads and builds each asset from fsys per request, so edits show up without a
// restart.
type Server struct {
	fsys         fs.FS
	files        map[string]*asset
	cacheControl string
	reload       bool
}

// Option configures a Server at construction time. It is the single extension
// point of the package: future features (a caching policy, minification, etc.)
// arrive as Options instead of changing New's signature.
type Option func(*Server)

// WithCacheControl sets the Cache-Control header sent with every asset. It is
// empty by default: revalidation is handled by ETag/304 without imposing a
// caching policy on callers.
func WithCacheControl(value string) Option {
	return func(s *Server) { s.cacheControl = value }
}

// WithReload, when enabled, makes the server read and rebuild each asset from
// fsys on every request instead of precomputing them in New. It trades the
// in-memory cache for picking up edits without a restart, so it is meant for
// development; leave it off (the default) in production.
func WithReload(enabled bool) Option {
	return func(s *Server) { s.reload = enabled }
}

// New reads every file in fsys into memory and precomputes its served form.
func New(fsys fs.FS, opts ...Option) (*Server, error) {
	s := &Server{fsys: fsys, files: make(map[string]*asset)}
	for _, opt := range opts {
		opt(s)
	}

	if s.reload {
		return s, nil // assets are read and built per request; nothing to precompute
	}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		s.files[p] = newAsset(p, raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ServeHTTP answers a request by looking the path up in memory. It writes 404
// when the file is unknown, 304 when the client's ETag still matches, and
// otherwise the (optionally gzip-encoded) body.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	a := s.lookup(name)
	if a == nil {
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	h.Set("Content-Type", a.contentType)
	h.Set("ETag", a.etag)
	h.Set("Vary", "Accept-Encoding")
	if s.cacheControl != "" {
		h.Set("Cache-Control", s.cacheControl)
	}

	if noneMatch(r.Header.Get("If-None-Match"), a.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := a.raw
	if a.gzip != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = a.gzip
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))

	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

// lookup returns the asset for name, or nil if it isn't a served file. With
// reload on it reads and builds the asset from fsys on the spot, so edits are
// reflected without a restart; otherwise it hits the precomputed map.
func (s *Server) lookup(name string) *asset {
	if !s.reload {
		return s.files[name]
	}
	raw, err := fs.ReadFile(s.fsys, name)
	if err != nil {
		return nil // missing, a directory, or an invalid path
	}
	return newAsset(name, raw)
}

// newAsset builds the served form of a file. This is the single place where an
// asset is assembled, so transforms added later (e.g. minifying text before
// hashing and compressing) have one obvious home.
func newAsset(name string, raw []byte) *asset {
	return &asset{
		contentType: contentType(name),
		etag:        makeETag(raw),
		raw:         raw,
		gzip:        compress(raw),
	}
}

// makeETag returns a strong ETag derived from the file's bytes. The same ETag
// is used for the raw and gzip forms; Vary: Accept-Encoding tells caches the
// representation depends on the request.
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
		return nil // didn't shrink (png/jpg/woff2…)
	}
	return bytes.Clone(buf.Bytes())
}

// webTypes pins the common web extensions to deterministic values, because
// mime.TypeByExtension consults the OS (the Windows registry, notably) and can
// return surprising results for .js or .css from one machine to another.
var webTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".xml":   "application/xml",
	".txt":   "text/plain; charset=utf-8",
	".ico":   "image/x-icon",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".webp":  "image/webp",
	".gif":   "image/gif",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".ttf":   "font/ttf",
	".eot":   "application/vnd.ms-fontobject",
}

func contentType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ct, ok := webTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
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
