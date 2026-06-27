// Package requestlog provides access-log middleware: one structured line per
// request (method, path, status, bytes, duration, client IP), at a level that
// follows the status class (2xx/3xx info, 4xx warn, 5xx error).
//
//	h := requestlog.New(logs.Access(),
//	    requestlog.WithClientIP(realIP),
//	    requestlog.WithSkipPaths("/health"),
//	)(mux)
//
// The line goes to a caller-supplied *slog.Logger, so where it lands (a file, an
// "access" sink, stdout) is the caller's choice. Static assets and noisy paths
// are skipped so the log stays about pages and API. Zero dependencies.
package requestlog

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// DefaultSkipExt are the static-asset extensions skipped by default: they are
// served by the static handler and would be noise in an access log. Override
// with WithSkipExt.
var DefaultSkipExt = []string{
	".css", ".js", ".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico",
	".woff", ".woff2", ".ttf", ".eot", ".webp", ".avif", ".map", ".xml", ".txt",
}

type config struct {
	skipPaths map[string]bool
	skipExt   map[string]bool
	clientIP  func(*http.Request) string
	requestID func(*http.Request) string
}

// Option configures the middleware at construction time.
type Option func(*config)

// WithSkipPaths skips logging for the given exact request paths (e.g. "/health",
// hit constantly by a healthcheck).
func WithSkipPaths(paths ...string) Option {
	return func(c *config) {
		for _, p := range paths {
			c.skipPaths[p] = true
		}
	}
}

// WithSkipExt replaces DefaultSkipExt with the given extensions (each including
// the leading dot, e.g. ".css"). Pass none to log everything.
func WithSkipExt(exts ...string) Option {
	return func(c *config) {
		c.skipExt = make(map[string]bool, len(exts))
		for _, e := range exts {
			c.skipExt[strings.ToLower(e)] = true
		}
	}
}

// WithClientIP sets how the client IP is derived from the request (e.g. a
// real-IP/trusted-proxy helper). Without it the raw RemoteAddr is logged, so the
// package stays free of any proxy-trust policy.
func WithClientIP(fn func(*http.Request) string) Option {
	return func(c *config) { c.clientIP = fn }
}

// WithRequestID, when set and returning a non-empty value, adds a "request_id"
// attribute to each line. It is the seam for request-id correlation; leave it
// unset to omit the field.
func WithRequestID(fn func(*http.Request) string) Option {
	return func(c *config) { c.requestID = fn }
}

// New returns access-log middleware writing to logger.
func New(logger *slog.Logger, opts ...Option) func(http.Handler) http.Handler {
	c := &config{
		skipPaths: make(map[string]bool),
		clientIP:  func(r *http.Request) string { return r.RemoteAddr },
	}
	c.skipExt = make(map[string]bool, len(DefaultSkipExt))
	for _, e := range DefaultSkipExt {
		c.skipExt[e] = true
	}
	for _, opt := range opts {
		opt(c)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.skipPaths[r.URL.Path] || c.skipExt[strings.ToLower(path.Ext(r.URL.Path))] {
				next.ServeHTTP(w, r)
				return
			}

			inicio := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			attrs := []slog.Attr{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Int64("dur_us", time.Since(inicio).Microseconds()),
				slog.String("ip", c.clientIP(r)),
			}
			if c.requestID != nil {
				if id := c.requestID(r); id != "" {
					attrs = append(attrs, slog.String("request_id", id))
				}
			}
			logger.LogAttrs(r.Context(), levelFor(rec.status), "request", attrs...)
		})
	}
}

// levelFor maps a status code to a log level: 5xx error, 4xx warn, rest info.
func levelFor(status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// recorder wraps the ResponseWriter to capture status and bytes written. It
// forwards Hijack/Flush/Unwrap so it stays transparent to downstream code that
// streams (SSE), upgrades the connection (WebSocket) or unwraps via
// http.ResponseController.
type recorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (rec *recorder) WriteHeader(code int) {
	if rec.wroteHeader {
		return
	}
	rec.status = code
	rec.wroteHeader = true
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Flush forwards to the underlying writer when it supports flushing.
func (rec *recorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying writer when it supports hijacking.
func (rec *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rec.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("requestlog: underlying ResponseWriter does not implement http.Hijacker")
}
