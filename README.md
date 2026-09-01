# learnnw

Hands-on learning repository for building network protocols from scratch in Go.
Instead of using the high-level abstractions directly, each package re-implements
a protocol on top of raw TCP (`net` package) to understand what really happens
on the wire.

## Structure

```
learnnw/
├── http-from-scratch/   # Go module: incremental HTTP implementations
│   ├── http0.9/         # HTTP/0.9 — request line only, body-only response
│   │   ├── server/      # Server type + connection loop
│   │   ├── response/    # responseBodyWriter (http.ResponseWriter impl)
│   │   ├── client/      # minimal raw-TCP HTTP/0.9 client
│   │   └── cmd/example/ # runnable demo
│   ├── http1/           # HTTP/1.0 — headers, status lines, request bodies
│   │   ├── server/
│   │   ├── response/
│   │   └── cmd/example/
│   ├── http1.1/         # HTTP/1.1 — persistent connections, chunked encoding
│   │   ├── server/
│   │   ├── response/
│   │   ├── chunked/     # Transfer-Encoding: chunked reader/writer
│   │   └── cmd/example/
│   └── internal/std/    # stdlib net/http exploration (benchmarks, middleware)
│       ├── mux/         # ServeMux benchmarks
│       └── server/      # middleware + graceful-shutdown helper
└── grpc-from-scratch/   # Go module: gRPC client with hand-written protobuf plumbing
    └── client/
        ├── example/     # Runnable example
        └── proto/       # .proto definition + generated Go code
```

`http-from-scratch` is a **single Go module** rooted at `github.com/kianooshaz/http-from-scratch`.
Run `go` commands from `http-from-scratch/`.

## Modules

### http-from-scratch

An incremental re-implementation of HTTP, one protocol version at a time.
Each protocol version is a small set of focused subpackages:

| Subpackage    | Purpose                                                             |
| ------------- | ------------------------------------------------------------------- |
| `server`      | The `Server` type, the accept loop, and the request parser.         |
| `response`    | An `http.ResponseWriter` that writes that protocol's response form. |
| `chunked`     | (HTTP/1.1) chunked-transfer-encoding reader (`Body`) and writer.    |
| `client`      | (HTTP/0.9) minimal raw-TCP client.                                  |
| `cmd/example` | Runnable demo wiring the above together with `net/http`.            |

`internal/std` holds experiments that lean on the standard library rather than
on raw sockets: `internal/std/mux` benchmarks `net/http`'s default `ServeMux`,
and `internal/std/server` packages a logging middleware + graceful shutdown.

Run an example:

```sh
cd http-from-scratch/http1.1/cmd/example
go run .
```

Run the mux benchmarks:

```sh
cd http-from-scratch
go test -bench . ./internal/std/mux
```

Build the whole module:

```sh
cd http-from-scratch
go build ./...
```

### grpc-from-scratch

A gRPC exercise following the [kmcd.dev "gRPC from scratch"](https://kmcd.dev)
series: a `GreetService` defined in `client/proto/greeter.proto`, generated Go
stubs, and a runnable Connect-based example in `client/example/`.

> **Note (WIP):** `client/example/server.go` imports `connectrpc.com/connect`
> and generated packages that aren't wired into `go.mod` yet. Run `go mod tidy`
> (after adding the imports) before running the example.

Regenerate the protobuf/gRPC code after editing `greeter.proto`:

```sh
cd grpc-from-scratch
protoc --go_out=. --go-grpc_out=. client/proto/greeter.proto
```

Run the example:

```sh
cd grpc-from-scratch/client/example
go mod tidy && go run server.go
```

## Requirements

- Go 1.25+
- `protoc` + `protoc-gen-go` / `protoc-gen-go-grpc` (only for regenerating gRPC code)
