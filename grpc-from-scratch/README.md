# grpc-from-scratch

A small, hand-written walk-through of gRPC: what the wire format looks
like, what the generated Go code is actually doing, and how the pieces
fit together.

This README explains gRPC end to end. The code in this folder is
written to be **read top-to-bottom** as a tutorial, with narrative
comments at every non-obvious step.

---

## What is gRPC?

gRPC is a **typed RPC framework** built on top of HTTP/2 and protobuf.
You write a service definition (a `.proto` file), a code generator
makes Go types and stubs, and your application calls remote methods as
if they were local function calls:

```go
resp, err := client.Greet(ctx, &proto.GreetRequest{Name: "world"})
// resp.Greeting == "Hello, world!"
```

The RPC framework handles:

| Concern              | What gRPC does                                |
| -------------------- | --------------------------------------------- |
| Serialization        | Protobuf (binary, schema-driven)              |
| Transport            | HTTP/2 (multiplexed streams)                  |
| Routing              | URL path derived from service/method names    |
| Errors               | Typed status codes in trailers                |
| Deadlines/cancel     | Context propagation, `grpc-timeout` header    |
| Streaming            | Unary, server-streaming, client-streaming, bidi |
| Compression          | Optional gzip / deflate on the payload        |
| Interoperability     | Other languages generate the same `.proto`   |

---

## What this folder contains

```
grpc-from-scratch/
├── proto/
│   ├── greeter.proto         ← the entire service definition (start here)
│   ├── greeter.pb.go         ← generated message types (read second)
│   └── greeter_grpc.pb.go    ← generated client/server stubs (read third)
├── client/
│   └── rawclient/            ← hand-written gRPC client: demystifies the wire
│       └── client.go
├── cmd/
│   └── example/main.go       ← runnable demo: server + how to invoke it
└── README.md                 ← this file
```

Reading order:

1. `proto/greeter.proto` — what a service definition looks like.
2. `proto/greeter.pb.go` — what `protoc-gen-go` emits from it.
3. `proto/greeter_grpc.pb.go` — what `protoc-gen-go-grpc` emits.
4. `cmd/example/main.go` — the smallest possible gRPC server.
5. `client/rawclient/client.go` — talk to a gRPC server *without* the
   gRPC library. After this you understand what grpc-go is doing
   under the hood.

---

## The wire protocol in one screen

A gRPC unary call is just an HTTP/2 POST with a few extra conventions:

```
Request headers:

  :method: POST
  :scheme: http
  :path: /greet.v1.GreetService/Greet
  :authority: localhost:9000
  content-type: application/grpc+proto
  te: trailers
  grpc-encoding: identity
  grpc-accept-encoding: identity,gzip
  user-agent: grpc-go/1.64.0

Request body:

  +---+---+---+---+---+---+---+---+---+---+ ... +---+
  | 0 |  length (uint32 big-endian)        |  payload   |
  +---+---+---+---+---+---+---+---+---+---+ ... +---+
    \____5-byte "Length-Prefixed Message" header__/

Response body: same shape.

Response trailers:

  grpc-status: 0
  grpc-message: (empty)
```

The HTTP/2 status is always 200, even on errors — the real status is in
the trailers (`grpc-status`). A non-zero `grpc-status` indicates a gRPC
error code (`OK=0`, `Cancelled=1`, `Unknown=2`, `InvalidArgument=3`,
`DeadlineExceeded=4`, …).

The 5-byte frame header is the only gRPC-specific framing on the body.
It's literally:

- byte 0 = compression flag (1 = compressed, 0 = uncompressed; almost
  always 0 in modern deployments).
- bytes 1..4 = payload length, big-endian uint32.

For more, see:
https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md

---

## How gRPC compares to plain HTTP

If you came here from `http-from-scratch/`, this table maps the gRPC
concepts onto the HTTP ones you already know:

| Concept                | Plain HTTP                         | gRPC                                       |
| ---------------------- | ---------------------------------- | ------------------------------------------ |
| Wire format            | `textproto` lines + freeform body  | HTTP/2 + length-prefixed protobuf frames   |
| Schema                 | none                               | `.proto` file                              |
| Serialization          | whatever you put in the body       | protobuf (binary)                          |
| Routing                | URL path your server parses        | `/<package>.<Service>/<Method>`            |
| Status                 | status line + headers              | HTTP 200 + `grpc-status` trailer           |
| Multiple requests      | one per connection (1.0) or shared | multiplexed over HTTP/2 streams            |
| Headers                | free-form text                     | strict, well-typed metadata                |
| Errors                 | status codes (200/404/500/…)       | typed status codes (16 of them)            |
| Streaming              | chunked transfer encoding          | four patterns (unary, server, client, bidi)|
| Cancellation           | close the conn                     | stream RST; context propagation           |
| Compression            | Content-Encoding                   | grpc-encoding + payload framing            |

In other words: gRPC is what you'd get if you redesigned HTTP from
scratch for typed RPCs. The trade-offs are real (binary-only,
HTTP/2-only, schema-locked-in) but so are the wins (performance,
type safety, codegen, language interop).

---

## What the proto file does — `proto/greeter.proto`

A `.proto` file declares:

1. A package namespace (`greet.v1`).
2. Messages — typed records (structs) (`GreetRequest`, `GreetResponse`).
3. A service — a named set of RPCs (`GreetService`).
4. Code-gen options (`go_package = "..."`).

Field numbers (the `= 1` after each field name) are part of the **wire
format**. They identify fields on the wire and must never change once a
schema ships. Renaming a Go field is fine; renumbering it breaks all
existing serialized payloads.

---

## What protoc-gen-go emits — `proto/greeter.pb.go`

`protoc-gen-go` takes the `.proto` and produces Go message types:

- Structs with the right fields.
- Getters (`GetName()`, `GetGreeting()`).
- Reflection metadata (`ProtoReflect()`, file descriptor bytes).
- Reset/String/Size methods.

Notice how most of the file is bookkeeping (reflection, descriptors).
The user-visible Go API is just two structs and two getters.

---

## What protoc-gen-go-grpc emits — `proto/greeter_grpc.pb.go`

`protoc-gen-go-grpc` produces:

- A client interface (`GreetServiceClient`) and a default implementation.
- A server interface (`GreetServiceServer`) that your service must satisfy.
- An `UnimplementedGreetServiceServer` for forward compatibility.
- A `RegisterGreetServiceServer` helper for `grpc.Server`.
- A `_GreetService_Greet_Handler` — the inner function that decodes the
  request, calls your method, and serializes the response.
- A `GreetService_ServiceDesc` registered with the gRPC server.

---

## What the example server does — `cmd/example/main.go`

A gRPC server is just a struct that implements the generated
`GreetServiceServer` interface:

```go
type GreetServer struct {
    proto.UnimplementedGreetServiceServer
}

func (s *GreetServer) Greet(_ context.Context, req *proto.GreetRequest) (*proto.GreetResponse, error) {
    return &proto.GreetResponse{
        Greeting: fmt.Sprintf("Hello, %s!", req.GetName()),
    }, nil
}
```

`UnimplementedGreetServiceServer` is embedded for forward compatibility:
if the `.proto` grows a new RPC and you haven't implemented it yet,
calls to the new method return a clean "Unimplemented" error instead of
panicking.

The example starts a `grpc.Server` on `:9000` and a plain HTTP `/healthz`
server on `:9001` so curl can probe the listener without any gRPC
tooling.

---

## What the raw client shows — `client/rawclient/client.go`

This package talks gRPC **without** using the gRPC library. It's the
direct counterpart to `http-from-scratch/http0.9/client/client.go`,
which talks HTTP/0.9 without using `net/http`.

Read it for:

- How to dial HTTP/2 in cleartext (h2c) using `net/http2.Transport`.
- The exact byte layout of a gRPC frame (5-byte header + protobuf).
- The headers a gRPC client must send (`content-type: application/grpc+proto`,
  `te: trailers`, `grpc-encoding`, …).
- How `grpc-status` from the response trailers is the real RPC status.

It is **not** a production-ready client. Real gRPC clients do a lot more
(deadline propagation, retry/backoff, load balancing, compression,
streaming, codec registration). But after reading it, the generated
client in `greeter_grpc.pb.go` will not feel like magic.

---

## How to run things

This folder is currently a **tutorial**, not a runnable workspace. The
proto/generated files in `proto/` were generated by an older setup and
this folder doesn't yet have a `go.sum`. To run the example end-to-end:

```sh
# Regenerate the proto and grpc stubs into ./proto:
cd grpc-from-scratch
protoc --go_out=. --go-grpc_out=. proto/greeter.proto

# Tidy and run the example:
go mod tidy
go run ./cmd/example
```

Then probe it:

```sh
# HTTP /healthz works without gRPC tooling:
curl -i http://localhost:9001/healthz

# gRPC probe with grpcurl:
grpcurl -plaintext localhost:9000 greet.v1.GreetService/Greet \
    -d '{"name":"world"}'
# → {"greeting":"Hello, world!"}
```

---

## Requirements

- Go 1.25+
- `protoc` + `protoc-gen-go` + `protoc-gen-go-grpc` (only for
  regenerating proto code).
- `grpcurl` (optional, for command-line RPC probing).

---

## Further reading

- [gRPC over HTTP/2 spec](https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md)
- [protobuf encoding spec](https://protobuf.dev/programming-guides/encoding/)
- [gRPC Go Quick Start](https://grpc.io/docs/languages/go/quickstart/)
- [Connect protocol](https://connectrpc.com/docs/protocol/) —
  Connect is a small, ergonomic variation on gRPC that uses regular
  HTTP/1.1 + JSON or HTTP/2 + protobuf. It interoperates with gRPC.
- [kmcd.dev "gRPC from scratch"](https://kmcd.dev) — the series this
  folder was originally modeled after.
