package view

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type datos struct{ Nombre string }

func motor(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	e, err := New(os.DirFS("testdata"), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func get(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestRenderBasico(t *testing.T) {
	e := motor(t)
	w := httptest.NewRecorder()
	if err := e.Render(w, get(nil), "index", datos{Nombre: "Ana"}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Hola Ana") || !strings.Contains(body, "pie compartido") {
		t.Errorf("la página no incluye el contenido y el parcial: %s", body)
	}
	if ct := w.Header().Get("Content-Type"); ct != contentType {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

func TestPaginaNoEncontrada(t *testing.T) {
	e := motor(t)
	if err := e.Render(httptest.NewRecorder(), get(nil), "no-existe", nil); err == nil {
		t.Fatal("una página inexistente debería devolver error")
	}
}

func TestCacheETag304(t *testing.T) {
	e := motor(t)

	// Primer render con cache: setea ETag.
	w1 := httptest.NewRecorder()
	if err := e.Render(w1, get(nil), "index", datos{Nombre: "Ana"}, WithCache("index:ana")); err != nil {
		t.Fatalf("Render: %v", err)
	}
	etag := w1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("el render cacheado debería setear ETag")
	}

	// Segundo render con el mismo ETag: 304 sin body.
	w2 := httptest.NewRecorder()
	if err := e.Render(w2, get(map[string]string{"If-None-Match": etag}), "index", datos{Nombre: "Ana"}, WithCache("index:ana")); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if w2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, quiere 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Errorf("un 304 no debería tener body, tiene %d bytes", w2.Body.Len())
	}
}

func TestCacheHitNoRerenderiza(t *testing.T) {
	e := motor(t)

	// Primer render con Nombre=Ana bajo una key.
	w1 := httptest.NewRecorder()
	_ = e.Render(w1, get(nil), "index", datos{Nombre: "Ana"}, WithCache("k"))

	// Segundo render con la MISMA key pero otro dato: debe servir el cacheado
	// (Ana), no re-renderizar con Beto. Esto verifica que el hit saltea el render.
	w2 := httptest.NewRecorder()
	_ = e.Render(w2, get(nil), "index", datos{Nombre: "Beto"}, WithCache("k"))
	if !strings.Contains(w2.Body.String(), "Hola Ana") {
		t.Errorf("un cache hit debería servir el output guardado (Ana), got: %s", w2.Body.String())
	}
}

func TestGzipNegociado(t *testing.T) {
	e := motor(t)
	w := httptest.NewRecorder()
	if err := e.Render(w, get(map[string]string{"Accept-Encoding": "gzip"}), "otra", nil, WithCache("otra")); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if enc := w.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Errorf("Content-Encoding = %q, quiere gzip", enc)
	}
	if v := w.Header().Get("Vary"); !strings.Contains(v, "Accept-Encoding") {
		t.Errorf("Vary = %q", v)
	}
}

func TestSinGzipSiNoSeAcepta(t *testing.T) {
	e := motor(t)
	w := httptest.NewRecorder()
	_ = e.Render(w, get(nil), "otra", nil, WithCache("otra"))
	if enc := w.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("sin Accept-Encoding no debería comprimir, got %q", enc)
	}
}

func TestSinCacheNoSeteaETag(t *testing.T) {
	e := motor(t)
	w := httptest.NewRecorder()
	_ = e.Render(w, get(nil), "index", datos{Nombre: "Ana"})
	if w.Header().Get("ETag") != "" {
		t.Error("sin WithCache no debería haber ETag")
	}
}

func TestReloadBypaseaCache(t *testing.T) {
	e := motor(t, WithReload(true))
	w := httptest.NewRecorder()
	_ = e.Render(w, get(nil), "index", datos{Nombre: "Ana"}, WithCache("k"))
	// En reload no se cachea ni se setea ETag (se relee/recompila por request).
	if w.Header().Get("ETag") != "" {
		t.Error("en modo reload no debería cachear ni setear ETag")
	}
	if !strings.Contains(w.Body.String(), "Hola Ana") {
		t.Error("en reload igual debe renderizar")
	}
}
