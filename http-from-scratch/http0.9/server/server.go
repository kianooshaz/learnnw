// Package server implements an HTTP/0.9 server.
//
// ─── What is HTTP/0.9? ──────────────────────────────────────────────────────
//
// HTTP/0.9 (1991) is the original HTTP, designed for fetching hypertext
// documents from a server. It was so simple that "HTTP" barely existed as
// a protocol: a request was just a single line, and the response was just
// the raw bytes of a file. There were no headers, no status codes, no
// methods besides GET, and no concept of a body separate from the document.
//
// A wire exchange looks like this (the client side is also raw TCP):
//
//	$ telnet example.com 80
//	Trying 93.184.216.34...
//	Connected to example.com.
//	Escape character is '^]'.
//	GET /index.html                                    ← single-line request
//	<!doctype html><html>Hello, world.</html>          ← raw body, no headers
//	Connection closed by foreign host.                 ← server hangs up
//
// That's the whole protocol. Three rules:
//
//  1. The request is a single ASCII line: "GET /path\r\n". The method is
//     always GET and there is no version string.
//  2. The response body is written directly to the connection, terminated
//     by the server closing the connection.
//  3. Each TCP connection handles exactly one request. There is no reuse.
//
// ─── What this package does ─────────────────────────────────────────────────
//
// It exposes a Server type that:
//
//   - listens on a TCP address;
//   - accepts a connection;
//   - reads one request line;
//   - synthesizes a minimal *http.Request with Proto = "HTTP/0.9";
//   - calls a user-supplied http.Handler with a response.Writer that just
//     dumps bytes to the conn; and
//   - closes the connection.
//
// It is a learning artefact, not a production server. net/http does not
// expose an HTTP/0.9 mode, so this is the only way to speak the protocol.
package server

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/kianooshaz/http-from-scratch/http0.9/response"
)

// Server is the entry point. Its shape mirrors net/http.Server so the rest
// of the codebase can swap a real net/http.Server in transparently.
type Server struct {
	Addr    string       // TCP address to listen on, e.g. ":9000".
	Handler http.Handler // User-supplied handler; defaults to DefaultServeMux.
}

// ListenAndServe blocks while accepting connections. Each connection is
// handled on its own goroutine, matching net/http's concurrency model.
func (s *Server) ListenAndServe() error {
	if s.Handler == nil {
		// Sensible default so callers that pass &Server{Addr: addr} still work.
		s.Handler = http.DefaultServeMux
	}

	// net.Listen returns a TCP socket bound to s.Addr. We hold the listener
	// for the lifetime of the server; defer Close so a graceful shutdown
	// path can be added later.
	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	defer l.Close()

	for {
		// Accept blocks until a client connects. A closed listener yields
		// net.ErrClosed; we treat that as a normal exit rather than an error.
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			slog.Error("http/0.9: accept failed", "err", err)
			return err
		}

		// Each connection is short-lived (single request) so we just spawn a
		// goroutine and let it run to completion.
		go func() {
			defer conn.Close()
			if err := s.handle(conn); err != nil && !errors.Is(err, io.EOF) {
				slog.Error("http/0.9: connection error", "err", err)
			}
		}()
	}
}

// handle reads one HTTP/0.9 request from conn and dispatches it.
//
// HTTP/0.9 gives us almost nothing to parse: just one line. The rest of
// the protocol surface (method, URL, version) is synthesized.
func (s *Server) handle(conn net.Conn) error {
	// bufio.Reader is needed because ReadLine scans ahead for "\n". A raw
	// net.Conn delivers arbitrary chunks; without buffering we could see
	// a partial line and mis-parse it.
	reader := bufio.NewReader(conn)
	line, _, err := reader.ReadLine()
	if err != nil {
		return err
	}

	// The request line is "GET /path" — split on whitespace. We don't care
	// about additional fields because HTTP/0.9 doesn't have them.
	fields := strings.Fields(string(line))
	if len(fields) < 2 {
		return errors.New("invalid request line")
	}

	// Synthesize the *http.Request. We pick the bits that matter:
	//   - Method:   from the request line (always GET in real HTTP/0.9,
	//               but we trust the wire here to keep the example honest).
	//   - URL:      a minimal url.URL with the path.
	//   - Proto:    set explicitly so handlers can branch on r.Proto.
	//   - RemoteAddr: useful for logging and rate limiting.
	req := &http.Request{
		Method:     fields[0],
		URL:        &url.URL{Scheme: "http", Path: fields[1]},
		Proto:      "HTTP/0.9",
		ProtoMajor: 0,
		ProtoMinor: 9,
		RemoteAddr: conn.RemoteAddr().String(),
	}

	// response.Writer is the simplest possible http.ResponseWriter: it
	// implements Write by writing straight to the conn, and treats Header
	// and WriteHeader as no-ops (HTTP/0.9 has no headers or status line).
	s.Handler.ServeHTTP(response.NewWriter(conn), req)
	return nil
}
