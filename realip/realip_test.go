package realip

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// req arma un *http.Request con la RemoteAddr y los headers dados.
func req(remote, xff, real string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set(XForwardedFor, xff)
	}
	if real != "" {
		r.Header.Set(XRealIP, real)
	}
	return r
}

func TestSinPoliticaDevuelveRemoteAddr(t *testing.T) {
	e := New() // sin WithTrusted: nunca confía
	if got := e.IP(req("1.2.3.4:5", "9.9.9.9", "")); got != "1.2.3.4" {
		t.Errorf("sin política debería devolver RemoteAddr, got %q", got)
	}
}

func TestPoliticaNilIgnoraHeaders(t *testing.T) {
	// WithTrusted(nil) resetea la política a "nunca confiar".
	e := New(WithTrustAll(), WithTrusted(nil))
	if got := e.IP(req("1.2.3.4:5", "9.9.9.9", "")); got != "1.2.3.4" {
		t.Errorf("nil CIDR debería ignorar headers, got %q", got)
	}
}

func TestFueraDelCIDRCaeARemoteAddr(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	e := New(WithTrusted(cidr))
	// El peer 1.2.3.4 está fuera del CIDR => no confiar en XFF forjado.
	if got := e.IP(req("1.2.3.4:5", "9.9.9.9", "")); got != "1.2.3.4" {
		t.Errorf("peer fuera del CIDR debería caer a RemoteAddr, got %q", got)
	}
}

func TestDentroDelCIDRTomaXFF(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	e := New(WithTrusted(cidr))
	// El peer 10.0.0.1 (Caddy) está dentro: se honra el XFF del cliente.
	if got := e.IP(req("10.0.0.1:5", "203.0.113.7", "")); got != "203.0.113.7" {
		t.Errorf("peer confiable debería tomar XFF, got %q", got)
	}
}

func TestXFFCadenaTomaIzquierdo(t *testing.T) {
	e := New(WithTrustAll())
	got := e.IP(req("10.0.0.1:5", "203.0.113.7, 10.0.0.1, 10.0.0.2", ""))
	if got != "203.0.113.7" {
		t.Errorf("debería tomar el izquierdo del XFF, got %q", got)
	}
}

func TestXFFMaloCaeARemoteAddr(t *testing.T) {
	e := New(WithTrustAll())
	// Valor que no parsea como IP => no usarlo.
	if got := e.IP(req("10.0.0.1:5", "no-es-ip", "")); got != "10.0.0.1" {
		t.Errorf("XFF inválido debería caer a RemoteAddr, got %q", got)
	}
}

func TestXFFVacioCaeARemoteAddr(t *testing.T) {
	e := New(WithTrustAll())
	if got := e.IP(req("10.0.0.1:5", "", "")); got != "10.0.0.1" {
		t.Errorf("XFF vacío debería caer a RemoteAddr, got %q", got)
	}
}

func TestPrioridadDeHeaders(t *testing.T) {
	// Con orden [X-Real-IP, X-Forwarded-For], Real-IP gana aunque XFF exista.
	e := New(WithTrustAll(), WithHeaders(XRealIP, XForwardedFor))
	got := e.IP(req("10.0.0.1:5", "203.0.113.7", "198.51.100.9"))
	if got != "198.51.100.9" {
		t.Errorf("Real-IP debería tener prioridad, got %q", got)
	}
}

func TestCaeAlSiguienteHeaderVacio(t *testing.T) {
	// Real-IP vacío => se usa el siguiente, XFF.
	e := New(WithTrustAll(), WithHeaders(XRealIP, XForwardedFor))
	got := e.IP(req("10.0.0.1:5", "203.0.113.7", ""))
	if got != "203.0.113.7" {
		t.Errorf("con Real-IP vacío debería caer a XFF, got %q", got)
	}
}

func TestWithTrustedCIDRInvalidoDejaSinPolitica(t *testing.T) {
	e := New(WithTrustedCIDR("no-es-cidr"))
	// CIDR inválido => política nil => RemoteAddr.
	if got := e.IP(req("1.2.3.4:5", "9.9.9.9", "")); got != "1.2.3.4" {
		t.Errorf("CIDR inválido debería dejar sin política, got %q", got)
	}
}

func TestWithTrustFuncNilEsIgnorado(t *testing.T) {
	e := New(WithTrustFunc(nil))
	if got := e.IP(req("1.2.3.4:5", "9.9.9.9", "")); got != "1.2.3.4" {
		t.Errorf("func nil debería ignorarse, got %q", got)
	}
}

func TestMiddlewareReescribeRemoteAddr(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	e := New(WithTrusted(cidr))

	var visto string
	h := e.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		visto = r.RemoteAddr
	}))
	h.ServeHTTP(httptest.NewRecorder(), req("10.0.0.1:5", "203.0.113.7", ""))

	if visto != "203.0.113.7" {
		t.Errorf("el handler debería ver la IP real, got %q", visto)
	}
}

func TestMiddlewareNoReescribeSiNoConfia(t *testing.T) {
	e := New() // sin política
	var visto string
	h := e.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		visto = r.RemoteAddr
	}))
	h.ServeHTTP(httptest.NewRecorder(), req("1.2.3.4:5", "9.9.9.9", ""))

	if visto != "1.2.3.4:5" {
		t.Errorf("sin política no debería tocar RemoteAddr, got %q", visto)
	}
}