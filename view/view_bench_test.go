package view

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func motorBench(b *testing.B) *Engine {
	b.Helper()
	e, err := New(os.DirFS("testdata"))
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return e
}

// BenchmarkRenderMiss mide el camino completo de render: ejecutar la plantilla,
// copiar el output, calcular ETag y gzip. Es el costo de la primera vez (o de
// cada request si no se cachea).
func BenchmarkRenderMiss(b *testing.B) {
	e := motorBench(b)
	d := datos{Nombre: "Ana"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Una key distinta por iteración fuerza siempre miss (mide el render).
			ent, err := e.construir("index", d)
			if err != nil {
				b.Fatal(err)
			}
			_ = ent
		}
	})
}

// BenchmarkRenderHit mide el camino servido desde cache: lookup + headers +
// escritura. Es el costo del caso común una vez calentado el cache.
func BenchmarkRenderHit(b *testing.B) {
	e := motorBench(b)
	// Calienta el cache una vez.
	_ = e.Render(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil),
		"index", datos{Nombre: "Ana"}, WithCache("k"))

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for pb.Next() {
			w := httptest.NewRecorder()
			if err := e.Render(w, r, "index", datos{Nombre: "Ana"}, WithCache("k")); err != nil {
				b.Fatal(err)
			}
		}
	})
}
