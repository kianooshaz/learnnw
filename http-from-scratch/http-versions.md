# HTTP versions — features at a glance

## HTTP/0.9 (1991)

- One method: `GET` only
- Request is a single line: `GET /path\r\n`
- No headers, no status line — response is raw bytes
- No content type, no error codes (client can't tell 200 from 404)
- Body ends when the server closes the connection
- One request per connection, no reuse

## HTTP/1.0 (1996, RFC 1945)

- Version string in the request line: `GET /path HTTP/1.0`
- More methods: `POST`, `HEAD`, `PUT`, `DELETE`, …
- Status line on responses: `HTTP/1.0 200 OK`
- Headers on both requests and responses (`Content-Type`, `User-Agent`, cookies, auth…)
- `Content-Length` header frames the body instead of connection close
- Status codes for success, redirect, and errors (2xx / 3xx / 4xx / 5xx)
- Still one request per connection

## HTTP/1.1 (1997/1999, RFC 2068/2616)

- Persistent connections by default — many requests on one TCP connection
- `Connection: close` to opt out of keep-alive
- Chunked transfer encoding — stream bodies of unknown length
- Mandatory `Host` header — virtual hosting, many sites on one IP
- More methods: `OPTIONS`, `PATCH`, `TRACE`, `CONNECT`
- Request bodies can also be chunked
- Response can be chunked when length isn't known up front
- Content negotiation (`Accept`, `Accept-Language`, …)
- Byte-range requests (`Range` header), caching controls (`Cache-Control`, `ETag`)
- Request pipelining (send several requests without waiting for replies)

## HTTP/3 (2022, RFC 9114) — implemented in `http3/`, over QUIC streams

- Binary protocol: messages are frames (type + length varint + payload)
- QUIC transport instead of TCP: UDP + TLS 1.3 built into the handshake
- One request per stream; many streams per connection, no head-of-line blocking
- Body framing disappears: DATA frames + the stream FIN replace
  Content-Length and chunked encoding
- Keep-alive disappears: a multi-stream connection is inherently persistent
- Request line becomes pseudo-headers: `:method`, `:scheme`, `:authority`, `:path`
- Header compression via QPACK (this repo: static table + literals, dynamic table off)
- Control streams: each peer sends a SETTINGS frame before anything else
- In this codebase the QUIC transport and QPACK come from the
  `quic-go`/`qpack` libraries; frames, streams, and the server state
  machine are hand-written

## HTTP/2 (2015, RFC 7540) — not implemented in this repo

- Binary protocol instead of text
- Multiplexing: many parallel streams over one connection
- Header compression (HPACK)
- Server push
