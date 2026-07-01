// Package realip derives the client IP behind a reverse proxy.
//
// Without a trust policy, any header is forgeable: a client can send its own
// X-Forwarded-For and pretend to be anyone. So the package never trusts
// headers unconditionally by default — it falls back to r.RemoteAddr until the
// caller states, via an option, who is allowed to set headers (a CIDR covering
// the trusted proxy, or "trust all" for development).
//
//	realIP := realip.New(realip.WithTrusted(proxyCIDR))            // production
//	logIP  := realip.New(realip.WithTrustAll())                    // dev / behind Caddy
//
//	// Plug into requestlog (one place reads the IP):
//	h := requestlog.New(logs.Access(),
//	    requestlog.WithClientIP(realIP.IP),
//	)(mux)
//
// Or run it once as middleware so every downstream reader of r.RemoteAddr sees
// the resolved IP:
//
//	h := realIP.Middleware(mux)
//
// The families consulted (X-Forwarded-For, X-Real-IP, CF-Connecting-IP, …) are
// configurable in priority order; the first non-empty one whose value parses
// as an IP wins, and for the comma-chain form (XFF) the leftmost token is taken
// as the original client. Zero dependencies — standard library only.
package realip

import (
	"net"
	"net/http"
	"strings"
)

// Well-known header families. Exported so callers can name them in WithHeaders
// without reaching for string literals. Which of these an app honors is a
// deployment decision: only list the ones the proxy/CDN in front actually sets,
// in priority order.
const (
	XForwardedFor  = "X-Forwarded-For" // RFC-style chain; leftmost entry is the original client.
	XRealIP        = "X-Real-IP"       // Nginx: a single IP.
	CFConnectingIP = "CF-Connecting-IP" // Cloudflare: a single IP.
	TrueClientIP   = "True-Client-IP"  // Akamai / Cloudflare enterprise: a single IP.
	XClientIP      = "X-Client-IP"     // Some proxies: a single IP.
)

// DefaultHeaders is the family consulted in priority order when none is
// configured: XFF alone, because it is the de-facto standard and what Caddy
// forwards. Add others (Real-IP, CF-Connecting-IP, …) only when the layer in
// front actually populates them.
var DefaultHeaders = []string{XForwardedFor}

type config struct {
	trust   func(remote string) bool
	headers []string
}

// Option configures the Extractor at construction time.
type Option func(*config)

// WithTrusted limits header trust to requests whose connection peer (the
// proxy) is inside the given CIDR. This is the production-safe mode: a client
// talking directly to the app cannot forge its own XFF because its remote
// address is outside the CIDR. Pass nil to ignore all headers and fall back to
// RemoteAddr — the default when no trust option is given.
func WithTrusted(n *net.IPNet) Option {
	return func(c *config) {
		if n == nil {
			c.trust = nil
			return
		}
		c.trust = func(remote string) bool {
			if ip := net.ParseIP(remote); ip != nil {
				return n.Contains(ip)
			}
			return false
		}
	}
}

// WithTrustedCIDR is a convenience for WithTrusted that parses the CIDR text.
// An invalid CIDR leaves the policy unset, so the caller validates the value
// (config time) rather than the package silently widening trust.
func WithTrustedCIDR(cidr string) Option {
	return func(c *config) {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return
		}
		WithTrusted(n)(c)
	}
}

// WithTrustAll trusts headers from any peer. Intended for development or when
// the app only ever sits behind a single trusted proxy that already sanitizes
// XFF (the current landing setup behind Caddy). In any public deployment prefer
// WithTrusted with the proxy's CIDR.
func WithTrustAll() Option {
	return func(c *config) { c.trust = func(string) bool { return true } }
}

// WithTrustFunc makes the trust decision a caller-supplied predicate over the
// connection peer (its parsed host). Used when the trusted set is not a single
// CIDR (e.g. a small list of proxy IPs).
func WithTrustFunc(f func(remote string) bool) Option {
	return func(c *config) {
		if f == nil {
			return
		}
		c.trust = f
	}
}

// WithHeaders overrides the families consulted (in priority order); the first
// non-empty one whose value parses as an IP wins. Defaults to DefaultHeaders.
func WithHeaders(names ...string) Option {
	return func(c *config) {
		c.headers = c.headers[:0]
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" {
				c.headers = append(c.headers, n)
			}
		}
	}
}

// Extractor resolves the client IP under a fixed trust policy and header
// priority list. Build one with New and reuse it across requests; it carries no
// per-request state.
type Extractor struct{ c config }

// New builds an Extractor from options. With none, it never trusts headers and
// always returns RemoteAddr — the safe default for an app not behind a proxy.
func New(opts ...Option) *Extractor {
	c := config{headers: append([]string(nil), DefaultHeaders...)}
	for _, opt := range opts {
		opt(&c)
	}
	return &Extractor{c: c}
}

// IP returns the real client IP for r under the extractor's policy. It is the
// seam plugged into requestlog.WithClientIP, and is safe from any handler:
// RemoteAddr is always known, so there is no error case.
func (e *Extractor) IP(r *http.Request) string {
	ip, _ := e.resolve(r)
	return ip
}

// resolve returns the real client IP and whether it came from a trusted header
// (true) rather than from r.RemoteAddr (false). The flag lets Middleware avoid
// rewriting RemoteAddr when there is nothing to add — a passthrough that keeps
// the original host:port form intact for code that expects it.
func (e *Extractor) resolve(r *http.Request) (ip string, fromHeader bool) {
	remote := remoteHost(r.RemoteAddr)
	if e.c.trust == nil || !e.c.trust(remote) {
		return remote, false // no policy or untrusted connection: ignore client-supplied headers
	}
	for _, h := range e.c.headers {
		v := strings.TrimSpace(r.Header.Get(h))
		if v == "" {
			continue
		}
		// XFF is a comma-separated chain of "client, proxy1, proxy2"; everything
		// else here is a single IP. The leftmost token of any header covers both
		// shapes, so split on the first comma and trim.
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if net.ParseIP(v) != nil {
			return v, true
		}
	}
	return remote, false
}

// Middleware resolves the client IP for each request and writes it back to
// r.RemoteAddr before delegating downstream, so any code reading r.RemoteAddr
// (requestlog, handlers) sees the real IP without each one wiring in WithClientIP.
// RemoteAddr is overwritten only when the IP came from a trusted header; when
// the connection is untrusted or no header was set the original host:port form
// is preserved untouched.
func (e *Extractor) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip, ok := e.resolve(r); ok {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}

// remoteHost extracts the host from a RemoteAddr of the form "host:port"
// (IPv4, IPv6 or a bare host). net.SplitHostPort handles both address forms; on
// parse failure the whole string is returned as the fallback.
func remoteHost(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}