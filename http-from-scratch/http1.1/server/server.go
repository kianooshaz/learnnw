// Package server implements an HTTP/1.1 server.
//
// ─── What HTTP/1.1 adds ─────────────────────────────────────────────────────
//
// RFC 2068 (1997) and later RFC 2616 (1999) defined HTTP/1.1 with three big
// improvements over 1.0:
//
//  1. Persistent connections. A single TCP connection carries an unbounded
//     sequence of request/response pairs. New requests are pipelined on
//     the same connection until either side sends Connection: close.
//
//  2. Chunked transfer encoding. When the response body length isn't known
//     in advance (e.g. streaming), the server can emit it as a series of
//     length-prefixed chunks instead of buffering to compute Content-Length.
//
//  3. Mandatory Host header. Multiple virtual hosts share one IP, so the
//     Host header is how the client picks which site it wants.
//
// A persistent-connection exchange:
//
//	→  GET /a HTTP/1.1\r\n            (request 1)
//	    Host: example.com\r\n
//	    Connection: keep-alive\r\n
//	←  HTTP/1.1 200 OK\r\n
//	    Transfer-Encoding: chunked\r\n
//	    \r\n
//	    5\r\nhello\r\n
//	    6\r\n world\r\n
//	    0\r\n\r\n
//	→  GET /b HTTP/1.1\r\n            (request 2 on same conn)
//	    Host: example.com\r\n
//	←  HTTP/1.1 200 OK\r\n
//	    ...
//	→  Connection: close\r\n          (or peer hangs up)
//
// ─── What this package does ─────────────────────────────────────────────────
//
//   - Listens on TCP and serves each connection on its own goroutine.
//   - Runs a per-connection loop reading requests and dispatching them.
//   - Parses request line + headers (bounded) + body (Content-Length or
//     chunked, bounded by EOF).
//   - Hands off to a user-supplied handler with a response.Writer that
//     speaks HTTP/1.1 (chunked by default, Connection header set).
package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	"github.com/kianooshaz/http-from-scratch/http1.1/chunked"
	"github.com/kianooshaz/http-from-scratch/http1.1/response"
)

// Server is the entry point. Its zero value is unusable; populate Addr and
// Handler before calling ListenAndServe.
type Server struct {
	Addr    string
	Handler http.Handler
}

// ListenAndServe blocks until the listener fails. Each accepted connection
// runs in its own goroutine.
func (s *Server) ListenAndServe() error {
	if s.Handler == nil {
		s.Handler = http.DefaultServeMux
	}

	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		go func() {
			if err := s.handle(conn); err != nil && !errors.Is(err, io.EOF) {
				fmt.Printf("http/1.1: connection error: %s\n", err)
			}
		}()
	}
}

// ─── Persistent connection loop ─────────────────────────────────────────────
//
// The connection loop reads requests until either:
//   - the peer closes the connection (clean EOF → return nil);
//   - the peer sends Connection: close;
//   - a malformed request is received (return error and close).
func (s *Server) handle(conn net.Conn) error {
	defer conn.Close()

	for {
		keepAlive, err := s.serveOne(conn)
		if err != nil {
			return err
		}
		if !keepAlive {
			return nil
		}
	}
}

// serveOne reads a single request from conn, dispatches it to the handler
// and flushes the response. It returns whether the connection should be
// kept alive for another request.
func (s *Server) serveOne(conn net.Conn) (bool, error) {
	// Bound headers to 1MB; lift the bound once headers are parsed so the
	// body can be any size (limited only by available memory).
	limitReader := io.LimitReader(conn, 1*1024*1024).(*io.LimitedReader)
	reader := bufio.NewReader(limitReader)
	headerReader := textproto.NewReader(reader)

	req, err := readRequest(headerReader, limitReader, reader, conn)
	if err != nil {
		return false, err
	}

	// Build the response writer. We hand it the *http.Request because the
	// writer uses req.Proto (for the status line), req.Close (to set the
	// Connection header), and triggers chunked encoding when the handler
	// doesn't set Content-Length or Transfer-Encoding itself.
	w := &response.Writer{
		Req:     req,
		Conn:    conn,
		Headers: make(http.Header),
	}

	s.Handler.ServeHTTP(w, req)

	// Flush terminates chunked encoding on this response (writes the
	// terminating "0\r\n\r\n" chunk if needed) and pushes any buffered
	// bytes. After this we may immediately read the next request.
	if err := w.Flush(); err != nil {
		return false, err
	}
	// req.Close is true if the client asked to close, false otherwise.
	// We invert it because the loop expects "keep going?".
	return !req.Close, nil
}

// readRequest parses one HTTP/1.1 request from headerReader.
//
// Compared to the HTTP/1.0 version this also:
//   - requires the Host header (mandatory in HTTP/1.1);
//   - honors Connection: close to set req.Close;
//   - recognizes Transfer-Encoding: chunked on the request body.
func readRequest(headerReader *textproto.Reader, limitReader *io.LimitedReader, reader *bufio.Reader, conn net.Conn) (*http.Request, error) {
	// ── Request line ──────────────────────────────────────────────────
	reqLine, err := headerReader.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("read request line: %w", err)
	}

	method, rest, ok := strings.Cut(reqLine, " ")
	if !ok {
		return nil, errors.New("invalid method")
	}
	if !validMethod(method) {
		return nil, errors.New("invalid method")
	}

	requestURI, proto, ok := strings.Cut(rest, " ")
	if !ok {
		return nil, errors.New("invalid request URI")
	}
	req := new(http.Request)
	if req.URL, err = url.ParseRequestURI(requestURI); err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}
	req.Method = method
	req.RequestURI = requestURI

	major, minor, ok := parseProtocol(proto)
	if !ok {
		return nil, errors.New("invalid protocol")
	}
	req.Proto = proto
	req.ProtoMajor = major
	req.ProtoMinor = minor

	// ── Header block ──────────────────────────────────────────────────
	// Loop until a blank line. Headers are keys lowercased to match
	// net/http's representation.
	req.Header = make(http.Header)
	for {
		line, err := headerReader.ReadLineBytes()
		if err != nil && err != io.EOF {
			return nil, err
		} else if err != nil {
			break
		}
		if len(line) == 0 {
			break
		}
		k, v, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			return nil, errors.New("invalid header")
		}
		req.Header.Add(strings.ToLower(string(k)), strings.TrimLeft(string(v), " "))
	}

	// ── Mandatory Host ────────────────────────────────────────────────
	// RFC 7230 §5.4: an HTTP/1.1 request MUST include a Host header.
	if _, ok := req.Header["Host"]; !ok {
		return nil, errors.New("required 'Host' header not found")
	}

	// ── Connection header ─────────────────────────────────────────────
	// "close"  → req.Close = true (one request, then close).
	// "keep-alive" or absent → req.Close = false (default for 1.1).
	switch strings.ToLower(req.Header.Get("Connection")) {
	case "keep-alive", "":
		req.Close = false
	case "close":
		req.Close = true
	}

	// Lift the header-size bound: from here on, the body can be any
	// size we have memory for.
	limitReader.N = math.MaxInt64

	// ── Context ───────────────────────────────────────────────────────
	// Handlers expect these context keys to be present; setting them
	// lets handler code be portable between this server and net/http.
	ctx := context.Background()
	ctx = context.WithValue(ctx, http.LocalAddrContextKey, conn.LocalAddr())
	req = req.WithContext(ctx)

	// ── Body ──────────────────────────────────────────────────────────
	// Three cases (in order of preference in HTTP/1.1):
	//   - Content-Length: fixed-length body of N bytes.
	//   - Transfer-Encoding: chunked: read via the chunked decoder.
	//   - Neither: empty body.
	contentLength, err := parseContentLength(req.Header.Get("Content-Length"))
	if err != nil {
		return nil, err
	}
	req.ContentLength = contentLength

	isChunked := req.Header.Get("Transfer-Encoding") == "chunked"
	switch {
	case contentLength == 0 && !isChunked:
		req.Body = noBody{}
	case isChunked:
		req.Body = &chunked.Body{Reader: reader}
	default:
		req.Body = &bodyReader{reader: io.LimitReader(reader, contentLength)}
	}

	req.RemoteAddr = conn.RemoteAddr().String()
	return req, nil
}

// parseContentLength: missing → 0; otherwise integer. We don't reject
// negative values (net/http has its own approach).
func parseContentLength(headerval string) (int64, error) {
	if headerval == "" {
		return 0, nil
	}
	return strconv.ParseInt(headerval, 10, 64)
}

// parseProtocol: HTTP/1.0 and HTTP/1.1 are both acceptable; our response
// line will always be HTTP/1.1 regardless of what the client sent.
func parseProtocol(proto string) (int, int, bool) {
	switch proto {
	case "HTTP/1.0":
		return 1, 0, true
	case "HTTP/1.1":
		return 1, 1, true
	}
	return 0, 0, false
}

// validMethod mirrors net/http's MethodGet..MethodTrace set.
func validMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// ─── Body helpers (same shape as the http1 package) ────────────────────────

type bodyReader struct{ reader io.Reader }

func (r *bodyReader) Read(p []byte) (int, error) { return r.reader.Read(p) }
func (r *bodyReader) Close() error {
	_, err := io.Copy(io.Discard, r.reader)
	return err
}

type noBody struct{}

func (noBody) Read([]byte) (int, error) { return 0, io.EOF }
func (noBody) Close() error             { return nil }
