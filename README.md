# go-httpx

A small toolkit of `net/http` utilities for building web apps with the Go
standard library. Not a framework: each package is independent and you import
only what you need.

> Status: **v0.x** — the API may still change between minor versions.

## Packages

| Package | What it does |
|---|---|
| [`static`](./static) | Serves static files from memory with ETag/304 and gzip. |
| [`log`](./log) | Named, asynchronous JSON loggers (`log/slog`) over caller-supplied sinks. |
| [`recover`](./recover) | Middleware: turns a handler panic into a 500 plus a structured log line. |
| [`requestlog`](./requestlog) | Middleware: one structured access-log line per request, level by status. |
| [`view`](./view) | Renders HTML pages over a shared layout, with optional ETag/304 + gzip output cache. |

Planned (not yet implemented): session helpers — added only where a thin wrapper
earns its place over using the upstream library directly.

## `static`

Reads an `fs.FS` once at startup and precomputes, per file, its content type,
ETag and gzip-compressed form. Serving is then a map lookup plus header
negotiation: no filesystem access and no per-request compression on the hot
path.

```go
import "github.com/gabrielxsuarez/go-httpx/static"

// Production: files on a mounted volume, served from memory.
srv, err := static.New(os.DirFS("/app/static"))

// Or baked into the binary.
//go:embed all:static
var embedded embed.FS
srv, err := static.New(embedded)

// Development: rebuild each file per request so edits show up without restart.
srv, err := static.New(os.DirFS("static"), static.WithReload(developing))

mux.Handle("GET /", srv)
```

Options:

- `WithReload(bool)` — read and rebuild assets per request (development).
- `WithCacheControl(string)` — set a `Cache-Control` header on every asset.

Features: strong ETag + conditional `304`, gzip negotiation (only when it
shrinks the file), deterministic content types, `HEAD` support. Zero
dependencies — standard library only.

## `log`

Named JSON loggers built on `log/slog`. Each name ("access", "events",
"warning", "error", or any custom one) gets its own logger backed by an async,
buffered writer: the caller only formats the line and hands it off; a single
background goroutine batches the writes behind a `bufio.Writer` and flushes on
an interval, keeping the write syscall off the hot path.

Where each log goes — and whether it rotates — is injected, so the package keeps
**zero third-party dependencies**: it writes to any `io.Writer`.

```go
import "github.com/gabrielxsuarez/go-httpx/log"

// Files on disk, rotated by lumberjack (the dependency lives in the app).
logs := log.New(func(name string) io.Writer {
    return &lumberjack.Logger{Filename: "logs/" + name + ".log", MaxSize: 100, MaxBackups: 5, Compress: true}
}, log.WithApp("my-app"))
defer logs.Close() // flush on graceful shutdown

logs.Access().LogAttrs(r.Context(), slog.LevelInfo, "request",
    slog.String("method", r.Method), slog.Int("status", status))
logs.Event().Info("demo solicitada", "email", email)

// Or everything to stdout, with rotation delegated to the container runtime.
logs := log.New(func(string) io.Writer { return os.Stdout })
```

Accessors: `Access()`, `Event()`, `Warning()`, `Error()` (also tees to stderr),
and `Named(name)` for anything else. Loggers are created lazily, so a sink is
only opened for a name actually used; returning `nil` disables that name.

Options:

- `WithApp(string)` — value of the `app` attribute on every line.
- `WithQueueSize(int)` / `WithBufferSize(int)` — channel depth / `bufio` size.
- `WithFlushInterval(time.Duration)` — how often the buffer is flushed.
- `WithBlockOnFull(bool)` — block instead of dropping when the queue is full
  (off by default; dropped lines are counted by `Dropped()`).
- `WithErrorStderr(bool)` — tee the error logger to stderr (on by default).

## `recover`

Middleware that recovers a panic from the wrapped handler: instead of `net/http`
dropping the connection and logging to its default `ErrorLog` (stderr, outside
the structured logs), it writes a `500` and logs `panic` — with the recovered
value, method, path and stack — to a caller-supplied `*slog.Logger`.
`http.ErrAbortHandler` is re-propagated unchanged. Zero dependencies.

```go
import "github.com/gabrielxsuarez/go-httpx/recover"

h := recover.New(logs.Error())(mux) // outermost, so it also covers other middleware
http.ListenAndServe(addr, h)
```

## `requestlog`

Middleware that writes one structured access-log line per request — method,
path, status, bytes, duration, client IP — to a caller-supplied `*slog.Logger`,
at a level that follows the status class (2xx/3xx info, 4xx warn, 5xx error).
Static-asset extensions (`DefaultSkipExt`) and configured paths are skipped so
the log stays about pages and API. The response recorder forwards
`Hijack`/`Flush`/`Unwrap`, so streaming and connection upgrades keep working.

```go
import "github.com/gabrielxsuarez/go-httpx/requestlog"

h := requestlog.New(logs.Access(),
    requestlog.WithClientIP(realIP),       // how to derive the client IP
    requestlog.WithSkipPaths("/health"),   // skip noisy paths
)(mux)
```

Options: `WithSkipPaths(...)`, `WithSkipExt(...)` (replaces `DefaultSkipExt`),
`WithClientIP(fn)`, `WithRequestID(fn)` (adds a `request_id` field when set).
Zero dependencies.

## `view`

Renders HTML pages with `html/template` over a shared layout. Each page is
compiled into its own cloned set, so pages can reuse block names (`head`,
`content`) without colliding. Templates come from an `fs.FS`, so the same code
serves them from a mounted volume (`os.DirFS`) or baked into the binary
(`embed.FS`).

Rendering with a cache key keeps the rendered output in memory with its ETag and
gzip form: repeat requests are a map lookup plus a conditional `304`, skipping
the render and the body transfer. Without a key the page is rendered every time
and served uncompressed. The page is assembled in a pooled buffer first, so a
template error never emits partial HTML.

```go
import "github.com/gabrielxsuarez/go-httpx/view"

e, err := view.New(os.DirFS("web"), view.WithReload(developing))

// Static page: cache by name. Variants: include them in the key.
e.Render(w, r, "about", nil, view.WithCache("about"))
e.Render(w, r, "home", data, view.WithCache(fmt.Sprintf("home:%t", data.Promo)))
```

Convention inside the `fs.FS`: `layout.html` (skeleton with `{{block "head" .}}`
/ `{{block "content" .}}`), `partial/*.html` (shared fragments), `*.html` (one
page each). Options: `WithReload(bool)`, `WithFuncs(template.FuncMap)`. Zero
dependencies.

## License

[MIT](./LICENSE)
