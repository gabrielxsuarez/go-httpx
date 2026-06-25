package static

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// bigCSS is large and compressible enough to clear gzipMinSize and shrink.
var bigCSS = strings.Repeat("body { margin: 0; padding: 0; }\n", 100)

func newServer(t *testing.T, files fstest.MapFS, opts ...Option) *Server {
	t.Helper()
	s, err := New(files, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func get(s *Server, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestServesRawWithHeaders(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"css/estilos.css": {Data: []byte(bigCSS)},
	})

	rec := get(s, http.MethodGet, "/css/estilos.css", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("ETag missing")
	}
	if v := rec.Header().Get("Vary"); v != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", v)
	}
	if rec.Header().Get("Content-Encoding") != "" {
		t.Error("Content-Encoding set without Accept-Encoding")
	}
	if rec.Body.String() != bigCSS {
		t.Error("body does not match source file")
	}
}

func TestGzipNegotiation(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"css/estilos.css": {Data: []byte(bigCSS)},
	})

	rec := get(s, http.MethodGet, "/css/estilos.css", map[string]string{
		"Accept-Encoding": "gzip",
	})

	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	if rec.Body.Len() >= len(bigCSS) {
		t.Errorf("gzip body (%d) not smaller than raw (%d)", rec.Body.Len(), len(bigCSS))
	}

	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	if string(got) != bigCSS {
		t.Error("decompressed body does not match source")
	}
}

func TestSmallFileNotCompressed(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"robots.txt": {Data: []byte("User-agent: *\n")}, // < gzipMinSize
	})

	rec := get(s, http.MethodGet, "/robots.txt", map[string]string{
		"Accept-Encoding": "gzip",
	})

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("small file was gzip-encoded: %q", enc)
	}
}

func TestConditionalNotModified(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"css/estilos.css": {Data: []byte(bigCSS)},
	})

	first := get(s, http.MethodGet, "/css/estilos.css", nil)
	etag := first.Header().Get("ETag")

	rec := get(s, http.MethodGet, "/css/estilos.css", map[string]string{
		"If-None-Match": etag,
	})

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 response carried a body of %d bytes", rec.Body.Len())
	}
}

func TestNotFound(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"css/estilos.css": {Data: []byte(bigCSS)},
	})

	rec := get(s, http.MethodGet, "/css/no-existe.css", nil)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHeadHasHeadersNoBody(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"css/estilos.css": {Data: []byte(bigCSS)},
	})

	rec := get(s, http.MethodHead, "/css/estilos.css", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("ETag missing on HEAD")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD response carried a body of %d bytes", rec.Body.Len())
	}
}

func TestContentTypeIsDeterministic(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"js/app.js": {Data: []byte(strings.Repeat("console.log(1);\n", 80))},
	})

	rec := get(s, http.MethodGet, "/js/app.js", nil)

	if ct := rec.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/javascript; charset=utf-8", ct)
	}
}

func TestReloadReflectsEdits(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "estilos.css")
	if err := os.WriteFile(file, []byte("/* v1 */"), 0o644); err != nil {
		t.Fatalf("write v1: %v", err)
	}

	s, err := New(os.DirFS(dir), WithReload(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if body := get(s, http.MethodGet, "/estilos.css", nil).Body.String(); body != "/* v1 */" {
		t.Fatalf("first read = %q, want /* v1 */", body)
	}

	if err := os.WriteFile(file, []byte("/* v2 */"), 0o644); err != nil {
		t.Fatalf("write v2: %v", err)
	}

	if body := get(s, http.MethodGet, "/estilos.css", nil).Body.String(); body != "/* v2 */" {
		t.Errorf("after edit = %q, want /* v2 */ (reload did not pick up the change)", body)
	}
}

func TestWithCacheControl(t *testing.T) {
	s := newServer(t, fstest.MapFS{
		"css/estilos.css": {Data: []byte(bigCSS)},
	}, WithCacheControl("public, max-age=86400"))

	rec := get(s, http.MethodGet, "/css/estilos.css", nil)

	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=86400" {
		t.Errorf("Cache-Control = %q", cc)
	}
}
