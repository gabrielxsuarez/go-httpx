package recover

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecuperaPanic(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	h := New(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, quiere 500", w.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, `"msg":"panic"`) || !strings.Contains(logged, "boom") {
		t.Errorf("el log no registró el panic: %s", logged)
	}
	if !strings.Contains(logged, `"path":"/x"`) {
		t.Errorf("el log no incluyó el path: %s", logged)
	}
}

func TestPasaSinPanic(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := New(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "ok")
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusTeapot || w.Body.String() != "ok" {
		t.Fatalf("la respuesta normal no pasó intacta: code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestRepropaganErrAbortHandler(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := New(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("se esperaba re-propagar ErrAbortHandler, recuperó %v", v)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
