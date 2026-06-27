// Package log provides a small set of named, asynchronous JSON loggers built on
// log/slog.
//
// Each name ("access", "events", "warning", "error", or any custom one) gets an
// independent slog.Logger backed by an async, buffered writer: the calling
// goroutine only formats the line and hands it off, while a single background
// goroutine batches writes behind a bufio.Writer and flushes them on an
// interval. This keeps the write syscall and disk latency off the hot path —
// the difference between a request log that costs a few percent of CPU and one
// that serializes every request on a file lock.
//
// The package has zero third-party dependencies: it writes to whatever
// io.Writer the caller supplies. Where each log goes — and whether it rotates —
// is a deployment decision, so the sink is injected, not baked in. The sink is a
// function from a log name to its writer, called once per name the first time
// that name is used:
//
//	// Files on disk, rotated by lumberjack (the dependency lives in the app).
//	logs := log.New(func(name string) io.Writer {
//		return &lumberjack.Logger{Filename: "logs/" + name + ".log", MaxSize: 100, MaxBackups: 5, Compress: true}
//	}, log.WithApp("my-app"))
//	defer logs.Close()
//
//	logs.Access().LogAttrs(r.Context(), slog.LevelInfo, "request",
//		slog.String("method", r.Method), slog.Int("status", status))
//
//	// Or everything to stdout, with rotation delegated to the container runtime.
//	logs := log.New(func(string) io.Writer { return os.Stdout })
//
// Loggers are created lazily on first use, so a sink is only opened for a name
// that is actually logged to. Returning nil from the sink disables that name
// (its logger discards). The "error" logger additionally tees to os.Stderr by
// default, so server errors stay visible even before the buffer is flushed.
//
// Close must be called on graceful shutdown — after the HTTP server has stopped
// serving — to flush buffered lines.
package log

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Well-known logger names. They are plain strings, so callers can use Named with
// any other name; these only back the typed accessors and the error/stderr tee.
const (
	NameAccess  = "access"
	NameEvent   = "events"
	NameWarning = "warning"
	NameError   = "error"
)

// Log manages a set of named loggers over caller-supplied sinks. It is safe for
// concurrent use: loggers are built once per name and then served from a
// lock-free map.
type Log struct {
	sink        func(name string) io.Writer
	app         string
	queueSize   int
	bufSize     int
	flushEvery  time.Duration
	block       bool
	errorStderr bool

	mu      sync.Mutex // serializes lazy creation
	loggers sync.Map   // name -> *slog.Logger (fast path)
	writers sync.Map   // name -> *asyncWriter (for Close / Dropped)
}

// Option configures a Log at construction time. It is the single extension
// point of the package: new behaviour arrives as Options instead of changing
// New's signature.
type Option func(*Log)

// WithApp sets the value of the "app" attribute added to every line. Empty by
// default (the attribute is omitted).
func WithApp(name string) Option { return func(l *Log) { l.app = name } }

// WithQueueSize sets how many lines may wait in the channel before the
// drop/block policy kicks in. Default 4096.
func WithQueueSize(n int) Option { return func(l *Log) { l.queueSize = n } }

// WithBufferSize sets the size in bytes of the bufio.Writer in front of each
// sink. Default 64 KiB.
func WithBufferSize(n int) Option { return func(l *Log) { l.bufSize = n } }

// WithFlushInterval sets how often the background goroutine flushes the buffer
// to its sink. Default 1s.
func WithFlushInterval(d time.Duration) Option { return func(l *Log) { l.flushEvery = d } }

// WithBlockOnFull, when enabled, makes a full queue block the caller until there
// is room instead of dropping the line. Off by default: dropping protects
// request latency under bursts, at the cost of losing some lines (counted by
// Dropped).
func WithBlockOnFull(enabled bool) Option { return func(l *Log) { l.block = enabled } }

// WithErrorStderr controls whether the "error" logger also writes to os.Stderr.
// On by default.
func WithErrorStderr(enabled bool) Option { return func(l *Log) { l.errorStderr = enabled } }

// New returns a Log whose loggers write to the sinks returned by sink. A nil
// sink defaults to os.Stdout for every name.
func New(sink func(name string) io.Writer, opts ...Option) *Log {
	l := &Log{
		sink:        sink,
		queueSize:   4096,
		bufSize:     64 << 10,
		flushEvery:  time.Second,
		block:       false,
		errorStderr: true,
	}
	for _, o := range opts {
		o(l)
	}
	if l.sink == nil {
		l.sink = func(string) io.Writer { return os.Stdout }
	}
	return l
}

// Access returns the logger for NameAccess.
func (l *Log) Access() *slog.Logger { return l.logger(NameAccess) }

// Event returns the logger for NameEvent — for operational/business events
// emitted from anywhere in the app.
func (l *Log) Event() *slog.Logger { return l.logger(NameEvent) }

// Warning returns the logger for NameWarning.
func (l *Log) Warning() *slog.Logger { return l.logger(NameWarning) }

// Error returns the logger for NameError (also tees to os.Stderr unless
// disabled with WithErrorStderr).
func (l *Log) Error() *slog.Logger { return l.logger(NameError) }

// Named returns the logger for an arbitrary name, creating it on first use.
func (l *Log) Named(name string) *slog.Logger { return l.logger(name) }

// Dropped returns the total number of lines dropped across all loggers because
// their queue was full (always 0 when WithBlockOnFull is set). Useful to log
// periodically as a health signal.
func (l *Log) Dropped() uint64 {
	var total uint64
	l.writers.Range(func(_, v any) bool {
		total += v.(*asyncWriter).dropped.Load()
		return true
	})
	return total
}

// Close flushes and stops every logger created so far. Call it once on graceful
// shutdown, after the server has stopped serving.
func (l *Log) Close() error {
	var first error
	l.writers.Range(func(_, v any) bool {
		if err := v.(*asyncWriter).Close(); err != nil && first == nil {
			first = err
		}
		return true
	})
	return first
}

// logger returns the logger for name, building it on first use. The fast path is
// a lock-free map load; only the first call for a given name takes the mutex.
func (l *Log) logger(name string) *slog.Logger {
	if v, ok := l.loggers.Load(name); ok {
		return v.(*slog.Logger)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if v, ok := l.loggers.Load(name); ok {
		return v.(*slog.Logger)
	}

	lg, aw := l.build(name)
	if aw != nil {
		l.writers.Store(name, aw)
	}
	l.loggers.Store(name, lg)
	return lg
}

// build assembles the logger for name: it resolves the sink, wraps it in the
// async writer (teeing the error logger to stderr), and attaches the shared
// attributes. A nil sink yields a logger that discards.
func (l *Log) build(name string) (*slog.Logger, *asyncWriter) {
	ws := l.sink(name)
	if ws == nil {
		return slog.New(slog.DiscardHandler), nil
	}

	aw := newAsyncWriter(ws, l.queueSize, l.bufSize, l.flushEvery, l.block)
	var w io.Writer = aw
	if name == NameError && l.errorStderr {
		// stderr is synchronous, so errors stay visible even before the async
		// buffer is flushed; the file copy still rides the async path.
		w = io.MultiWriter(os.Stderr, aw)
	}

	var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})

	attrs := make([]slog.Attr, 0, 2)
	if l.app != "" {
		attrs = append(attrs, slog.String("app", l.app))
	}
	attrs = append(attrs, slog.String("log", name))
	h = h.WithAttrs(attrs)

	return slog.New(h), aw
}
