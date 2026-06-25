# go-httpx

A small toolkit of `net/http` utilities for building web apps with the Go
standard library. Not a framework: each package is independent and you import
only what you need.

> Status: **v0.x** — the API may still change between minor versions.

## Packages

| Package | What it does |
|---|---|
| [`static`](./static) | Serves static files from memory with ETag/304 and gzip. |

Planned (not yet implemented): session helpers and a rotating `slog` handler —
added only where a thin wrapper earns its place over using the upstream library
directly.

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

## License

[MIT](./LICENSE)
