// Package recover provides middleware that turns a handler panic into a clean
// 500 response plus a structured log line, instead of letting net/http drop the
// connection and log to its default ErrorLog (stderr, outside the structured
// logs).
//
//	mux := http.NewServeMux()
//	// ... routes ...
//	h := recover.New(logger)(mux)
//	http.ListenAndServe(addr, h)
//
// The logger is any *slog.Logger, so the caller decides where panics go
// (typically an "error" sink). Zero dependencies — standard library only.
package recover

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// New returns middleware that recovers panics from the wrapped handler. On a
// panic it logs "panic" (with the recovered value, method, path and stack) to
// logger and writes a 500. http.ErrAbortHandler is re-propagated unchanged, so
// net/http's intentional-abort sentinel keeps working.
func New(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic",
					slog.Any("err", v),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				w.WriteHeader(http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}
