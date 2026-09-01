# http-from-scratch

An incremental re-implementation of HTTP on top of raw TCP, one protocol
version at a time. The goal is to *understand* what an HTTP server is
actually doing on the wire — every byte, every header, every state
machine — by writing it ourselves in a few hundred lines of Go.

This README is a guided tour of the HTTP protocol itself, with pointers to
the code that implements each stage.

---

## What is HTTP?

HTTP (Hypertext Transfer Protocol) is a **request/response** protocol that
runs on top of a reliable byte stream — almost always TCP. The shape is
the same in every version:

```
client                              server
  |  ─── open TCP connection ───►    |
  |  ─── send request  ─────────►    |
  |  ◄─── send response ─────────    |
  |  ─── close (or reuse) ────►     |
```

A request is "what does the client want?" A response is "here's the
answer, plus a status code that says whether you got it."

Everything else — headers, methods, status codes, chunked encoding,
keep-alive, routing, content negotiation — is a refinement layered on
top of that basic shape.

---

## The four versions in this repo

The codebase is organised so each version lives in its own directory and
you can read them in order:

```
http-from-scratch/
├── http0.9/    ← start here: 50 lines, the whole protocol
├── http1/      ← HTTP/1.0: status line and headers
├── http1.1/    ← HTTP/1.1: persistent connections and chunked encoding
└── internal/std/   ← contrast piece: real-world net/http patterns
```

Each protocol version follows the same internal shape:

```
<version>/
├── server/         # Server type, accept loop, request parser
├── response/       # http.ResponseWriter implementation for this version
├── chunked/        # (1.1 only) chunked-transfer-encoding reader and writer
├── client/         # (0.9 only) minimal raw-TCP client
└── cmd/example/    # runnable demo with probe routes
```

---

## HTTP/0.9 — `http0.9/`

**Read `http0.9/server/server.go` first.** It's ~100 lines and shows you
the entire protocol.

### The protocol in one screen

```
$ telnet example.com 80
GET /index.html

<!doctype html>Hello, world.
Connection closed by foreign host.
```

That's it. Three rules:

1. **Request**: a single ASCII line, `GET /path\r\n`.
2. **Response**: raw bytes written directly to the socket, no headers,
   no status line.
3. **Connection**: closed by the server after the body. No reuse.

### What this codebase does

- `http0.9/server/server.go` — `net.Listen` + accept loop. Reads one
  line, synthesises an `*http.Request` with `Proto = "HTTP/0.9"`,
  dispatches to a handler with a writer that just dumps bytes to the
  conn.
- `http0.9/response/response.go` — the smallest possible
  `http.ResponseWriter`. `Header()` returns `nil`, `WriteHeader()` is a
  no-op, only `Write()` does anything.
- `http0.9/client/client.go` — a raw TCP client so you can see the
  request/response cycle without involving `net/http`.
- `http0.9/cmd/example/main.go` — runnable demo. Try `curl --http0.9`
  against it.

### What HTTP/0.9 *can't* do

- No status codes (so the client can't tell 200 from 404 from 500).
- No headers (so no `Content-Type`, no cookies, no auth).
- No methods other than `GET`.
- One request per connection (so a page with 10 images costs 10 TCP
  handshakes).

All four limitations get addressed in HTTP/1.0.

---

## HTTP/1.0 — `http1/`

**Read `http1/server/server.go` next.** It's the same accept loop plus
a real request parser and a response writer that emits a status line.

### The protocol in one screen

```
GET /hello HTTP/1.0\r\n
Host: example.com\r\n
User-Agent: demo/1.0\r\n
\r\n
                                                ← end of headers
HTTP/1.0 200 OK\r\n
Content-Type: text/plain\r\n
Content-Length: 13\r\n
\r\n                                                ← end of response headers
Hello, world!                                     ← body, 13 bytes
                                                ← server closes conn
```

### What changed since 0.9

| Feature        | HTTP/0.9          | HTTP/1.0                              |
| -------------- | ----------------- | ------------------------------------- |
| Methods        | GET only          | GET, POST, HEAD, PUT, DELETE, …       |
| Status line    | none              | `HTTP/1.0 200 OK`                     |
| Headers        | none              | `Name: Value\r\n` block, both ways    |
| Content-Length | none (close = end)| exact byte count                      |
| Body framing   | connection close  | `Content-Length` header               |
| Connection     | one-shot          | one-shot, but documented              |

### What this codebase does

- `http1/server/server.go` — accept loop, plus `readRequest` which
  parses the request line, header block, and Content-Length body.
  Bounded header read (1 MB) for safety.
- `http1/response/response.go` — buffers headers until the first
  `WriteHeader` or `Write`, then emits the status line + headers +
  body. The same trick `net/http` uses internally.
- `http1/cmd/example/main.go` — one demo route. `curl -i` shows the
  full response shape.

### What HTTP/1.0 still can't do

Opening a fresh TCP connection per request costs roughly **1 RTT of
latency** before you even send a byte. For a page with N resources
that means N extra round-trips before any of them load. HTTP/1.1
fixes this.

---

## HTTP/1.1 — `http1.1/`

**Read `http1.1/server/server.go` for the connection loop, then
`http1.1/chunked/chunked.go` for chunked encoding.**

### The protocol in one screen

Two requests on the same connection:

```
→  GET /a HTTP/1.1\r\n
   Host: example.com\r\n
   Connection: keep-alive\r\n
   \r\n
←  HTTP/1.1 200 OK\r\n
   Transfer-Encoding: chunked\r\n
   \r\n
   5\r\nhello\r\n
   6\r\n world\r\n
   0\r\n\r\n
→  GET /b HTTP/1.1\r\n
   Host: example.com\r\n
   \r\n
←  HTTP/1.1 200 OK\r\n
   …
```

### What changed since 1.0

| Feature              | HTTP/1.0            | HTTP/1.1                                       |
| -------------------- | ------------------- | ---------------------------------------------- |
| Connections          | one-shot            | persistent by default                          |
| Keep-alive           | optional header     | default; opt out with `Connection: close`     |
| Body framing         | `Content-Length`    | `Content-Length` **or** `Transfer-Encoding: chunked` |
| Host header          | optional            | **mandatory** (virtual hosting)                |
| Chunked transfer     | not defined         | yes — stream bodies of unknown length          |
| Request pipelining   | not defined         | yes (clients can send multiple requests without waiting) |

### Persistent connections — the big win

A TCP handshake is roughly **1 round-trip**. TLS on top is another
**1–2 round-trips**. Opening a fresh connection per resource is the
single biggest reason HTTP/1.0 felt slow. HTTP/1.1's persistent
connection reuses the socket across requests — the cost is amortised
to zero after the first request.

In this codebase, see `Server.handle` in `http1.1/server/server.go`.
It loops over `serveOne` until the request says `Connection: close`.

### Chunked encoding — streaming without a length

`Content-Length` works only when the server knows the body size up
front. For streaming responses (chat messages, live logs, dynamic
content), the server can't know the size until it's done writing.

HTTP/1.1's answer is chunked encoding: the body is a sequence of
length-prefixed chunks, terminated by a zero-length chunk:

```
5\r\n
hello\r\n
6\r\n
 world\r\n
0\r\n
\r\n                ← terminating chunk + trailer separator
```

Each chunk: `<hex size>\r\n<size bytes>\r\n`. The `0\r\n\r\n` says
"body over; trailers (if any) end at the second CRLF."

In this codebase, see `http1.1/chunked/chunked.go`. The `Body` type
reads chunked request bodies (used when a client sends
`Transfer-Encoding: chunked`); the `Encoder` type writes chunked
response bodies (used when a handler doesn't set
`Content-Length`).

In `http1.1/response/response.go` look at how `Writer` decides
whether to chunk: it inspects `Headers["Content-Length"]` and
`Headers["Transfer-Encoding"]` and defaults to chunked when neither
is set. That's exactly what `net/http` does.

### What's still missing

- No multiplexing: a slow response blocks subsequent requests on the
  same connection. (HTTP/2 fixes this with binary framing and many
  streams over one connection.)
- Header compression: redundant headers (User-Agent, Cookie, …) are
  sent on every request. (HTTP/2 fixes this with HPACK.)
- Plaintext only by default (unless you wrap it in TLS).

---

## `internal/std/` — the contrast piece

After walking the protocol from scratch, look at `internal/std/` for
what a production HTTP server in Go actually looks like. It uses
`net/http` directly and shows:

- `internal/std/mux/mux_test.go` — benchmarks of `net/http`'s default
  `ServeMux` under different route shapes. Run with
  `go test -bench . ./internal/std/mux`.
- `internal/std/server/server.go` — middleware (`LoggingMiddleware`),
  graceful shutdown (`Serve`), sensible timeouts
  (`ReadTimeout`, `ReadHeaderTimeout`, …). All of these are things
  every production HTTP server needs and the from-scratch code in
  this repo deliberately omits, to keep the reading short.

---

## Reading order

If you want to learn HTTP this way:

1. `http0.9/server/server.go` — read the whole file top to bottom.
2. `http0.9/response/response.go`, `client/client.go`,
   `cmd/example/main.go` — short, fills in the picture.
3. Skim RFC 1945 (HTTP/1.0). Compare it to what you just read.
4. `http1/server/server.go` — focus on `readRequest` and the
   `LimitReader` trick for header bounding.
5. `http1/response/response.go` — note the header buffering pattern;
   this is exactly how `net/http` works.
6. RFC 2068 / 7230 / 7231 (HTTP/1.1).
7. `http1.1/server/server.go` — `handle` and `serveOne` for
   persistent connections.
8. `http1.1/chunked/chunked.go` — read `Body.Read` carefully.
9. `http1.1/response/response.go` — note how `ChunkedEncoding` is
   picked.
10. `internal/std/mux/mux_test.go` and `internal/std/server/server.go`
    — the "what real Go does" comparison.

---

## How to run things

```sh
# Pick a version, run its demo.
cd http0.9/cmd/example && go run .
cd ../../http1/cmd/example && go run .
cd ../../http1.1/cmd/example && go run .

# Probe the HTTP/0.9 server with curl:
curl --http0.9 http://127.0.0.1:9000/this/is/a/test

# Probe chunked request bodies against the HTTP/1.1 server:
curl -i -X POST -H 'Transfer-Encoding: chunked' \
    --data-binary @- http://127.0.0.1:9000/echo/chunked <<EOF
5
hello
0
EOF

# Build the whole module:
cd ../.. && go build ./...

# Run the stdlib benchmarks:
go test -bench . ./internal/std/mux
```

---

## Requirements

- Go 1.25+ (for the `signal.NotifyContext` and `bytes.Cut` patterns used
  in the code, and for `http.Request.PathValue` used in the HTTP/1.1
  example).

## Further reading

- RFC 1945 — HTTP/1.0
- RFC 7230, 7231, 7232, 7233, 7234, 7235 — HTTP/1.1 (now obsoleted by
  RFC 9110, but still the clearest write-up)
- RFC 7540 — HTTP/2
- "Go Web Programming" chapters on `net/http` internals
- Go blog: "HTTP/2 in Go" and "Routing improvements in Go 1.22"
