package requestlog

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// parseLines decodifica las líneas JSON escritas por el logger.
func parseLines(t *testing.T, s string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(s), "\n") {
		if ln == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("línea de log no es JSON: %q (%v)", ln, err)
		}
		out = append(out, m)
	}
	return out
}

func nuevo(buf *strings.Builder, opts ...Option) func(http.Handler) http.Handler {
	return New(slog.New(slog.NewJSONHandler(buf, nil)), opts...)
}

func TestLogueaStatusYBytes(t *testing.T) {
	var buf strings.Builder
	h := nuevo(&buf)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hola")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/pagina", nil))

	lines := parseLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("se esperaba 1 línea, hubo %d", len(lines))
	}
	l := lines[0]
	if l["status"].(float64) != 201 || l["bytes"].(float64) != 4 {
		t.Errorf("status/bytes mal: %v", l)
	}
	if l["method"] != "GET" || l["path"] != "/pagina" {
		t.Errorf("method/path mal: %v", l)
	}
	if _, ok := l["dur_us"]; !ok {
		t.Errorf("falta dur_us: %v", l)
	}
}

func TestStatusPorDefectoEs200(t *testing.T) {
	var buf strings.Builder
	h := nuevo(&buf)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sin WriteHeader explícito")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if parseLines(t, buf.String())[0]["status"].(float64) != 200 {
		t.Error("un Write sin WriteHeader debería registrar status 200")
	}
}

func TestNivelPorStatus(t *testing.T) {
	casos := []struct {
		status int
		nivel  string
	}{
		{200, "INFO"}, {301, "INFO"}, {404, "WARN"}, {500, "ERROR"},
	}
	for _, c := range casos {
		var buf strings.Builder
		status := c.status
		h := nuevo(&buf)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		if got := parseLines(t, buf.String())[0]["level"]; got != c.nivel {
			t.Errorf("status %d: nivel = %v, quiere %v", c.status, got, c.nivel)
		}
	}
}

func TestSkipPaths(t *testing.T) {
	var buf strings.Builder
	h := nuevo(&buf, WithSkipPaths("/health"))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("/health no debería loguearse, escribió: %s", buf.String())
	}
}

func TestSkipExtPorDefecto(t *testing.T) {
	var buf strings.Builder
	h := nuevo(&buf)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/estilos.css", nil))
	if strings.TrimSpace(buf.String()) != "" {
		t.Errorf("un .css no debería loguearse por defecto, escribió: %s", buf.String())
	}
}

func TestClientIP(t *testing.T) {
	var buf strings.Builder
	h := nuevo(&buf, WithClientIP(func(*http.Request) string { return "9.9.9.9" }))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if parseLines(t, buf.String())[0]["ip"] != "9.9.9.9" {
		t.Error("debería usar la IP inyectada por WithClientIP")
	}
}

func TestRequestIDOpcional(t *testing.T) {
	// Sin la opción no aparece el campo.
	var buf strings.Builder
	nuevo(&buf)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if _, ok := parseLines(t, buf.String())[0]["request_id"]; ok {
		t.Error("sin WithRequestID no debería haber request_id")
	}

	// Con la opción aparece.
	var buf2 strings.Builder
	nuevo(&buf2, WithRequestID(func(*http.Request) string { return "abc123" }))(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if parseLines(t, buf2.String())[0]["request_id"] != "abc123" {
		t.Error("con WithRequestID debería loguearse el id")
	}
}

// flushHijackRecorder simula un ResponseWriter que soporta Flush y Hijack, para
// verificar que el recorder los reenvía.
type flushHijackRecorder struct {
	*httptest.ResponseRecorder
	flushed  bool
	hijacked bool
}

func (f *flushHijackRecorder) Flush() { f.flushed = true }
func (f *flushHijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true
	return nil, nil, nil
}

func TestReenviaFlushHijackUnwrap(t *testing.T) {
	var buf strings.Builder
	var inner *flushHijackRecorder
	h := nuevo(&buf)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("el recorder debería implementar http.Flusher")
		}
		w.(http.Flusher).Flush()
		if _, ok := w.(http.Hijacker); !ok {
			t.Error("el recorder debería implementar http.Hijacker")
		}
		_, _, _ = w.(http.Hijacker).Hijack()
		if u, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok || u.Unwrap() != inner {
			t.Error("Unwrap debería devolver el writer subyacente")
		}
	}))
	inner = &flushHijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(inner, httptest.NewRequest(http.MethodGet, "/", nil))

	if !inner.flushed || !inner.hijacked {
		t.Errorf("Flush/Hijack no se reenviaron: flushed=%v hijacked=%v", inner.flushed, inner.hijacked)
	}
}
