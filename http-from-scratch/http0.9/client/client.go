// Package client is a minimal HTTP/0.9 client built on top of net.Conn.
//
// Most HTTP clients you'll ever meet (net/http, curl, …) speak HTTP/1.1
// and refuse to fall back to HTTP/0.9. Speaking 0.9 by hand is a useful
// exercise because it shows what an HTTP client is actually doing behind
// the scenes: open a TCP socket, write the request line, read until EOF.
//
// Wire format reminder:
//
//	request : "GET /path\r\n"        (single line, terminated by CRLF)
//	response: <raw bytes until EOF>  (no headers, no status line)
package client

import (
	"errors"
	"fmt"
	"io"
	"net"
)

// Get dials addr (host:port) and sends a single HTTP/0.9 GET for path.
// It returns the entire response body as a byte slice.
//
// The function is intentionally tiny so you can read it top-to-bottom in
// one screen. net/http's real client is ~5000 lines doing the same idea
// but with retries, TLS, redirects, streaming, content-length handling,
// chunked decoding, and so on.
func Get(addr, path string) ([]byte, error) {
	// Open a raw TCP socket. No TLS, no keep-alive — every call opens a
	// fresh connection, exactly mirroring what HTTP/0.9 expects.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	// Write the request: a single ASCII line terminated by CRLF. Note there
	// is no HTTP version string — that would make it HTTP/1.0.
	if _, err := fmt.Fprintf(conn, "GET %s\r\n", path); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Read until the server closes the connection. io.ReadAll returns
	// io.EOF on a clean close; we treat that as success because RFC says
	// "body terminated by connection close" — exactly what we just saw.
	body, err := io.ReadAll(conn)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return body, nil
}
