// Package response implements the HTTP/0.9 response writer.
//
// In HTTP/0.9 there are no response headers and no status line — the body
// is the entire response and the connection is closed when it ends.
// This package exposes the smallest possible http.ResponseWriter that
// preserves that on-the-wire shape:
//
//   - Write(p)        → conn.Write(p)               (body bytes)
//   - Header()        → nil                          (no headers)
//   - WriteHeader(n)  → no-op                        (no status line)
//
// Because http.ResponseWriter is an interface, returning *Writer from a
// helper keeps the call site clean: response.NewWriter(conn).
package response

import (
	"net"
	"net/http"
)

// Writer is the only type in this package. It is unexported intentionally
// (callers use NewWriter) which mirrors how the stdlib hides its concrete
// response writer.
type Writer struct {
	conn net.Conn
}

// NewWriter wraps a raw connection as an http.ResponseWriter.
//
// Typical callers will never need to call this directly — the HTTP/0.9
// server creates one for each request and passes it to the handler.
func NewWriter(c net.Conn) http.ResponseWriter {
	return &Writer{conn: c}
}

// Header is unsupported in HTTP/0.9; it always returns nil. Returning nil
// (rather than an empty http.Header) makes it obvious to readers that any
// attempt to set headers will be silently dropped.
func (w *Writer) Header() http.Header { return nil }

// WriteHeader is unsupported in HTTP/0.9; it is a no-op. We keep it on the
// type so Handler authors who call it (out of habit) don't panic.
func (w *Writer) WriteHeader(statusCode int) {}

// Write sends body bytes directly over the connection. There is no
// buffering, no chunked encoding, no Content-Length — when the handler
// returns, the server closes the connection and the client sees EOF.
func (w *Writer) Write(b []byte) (int, error) {
	return w.conn.Write(b)
}
