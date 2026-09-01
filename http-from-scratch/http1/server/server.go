// Package server implements an HTTP/1.0 server.
//
// ─── What did HTTP/1.0 add over HTTP/0.9? ───────────────────────────────────
//
// HTTP/0.9 was great for hypertext but not for much else. As the web grew,
// people wanted to send form data, return different document types, indicate
// errors, and (critically) tell the client what kind of document they were
// getting. RFC 1945 (1996) defined HTTP/1.0 to formalize that:
//
//   - Request lines now include a method and version:
//     GET /index.html HTTP/1.0
//   - Methods beyond GET: POST, HEAD, PUT, DELETE, etc.
//   - Header fields on both requests and responses, "Name: Value" lines.
//   - A status line on responses: "HTTP/1.0 200 OK".
//   - A blank line separates headers from the body.
//   - Content-Length header on responses so clients know when to stop.
//   - One request per connection: after the response, the server closes.
//
// A wire exchange for a simple GET:
//
//	GET /hello HTTP/1.0\r\n
//	Host: example.com\r\n
//	User-Agent: demo/1.0\r\n
//	\r\n
//	                                            ← end of headers
//	HTTP/1.0 200 OK\r\n
//	Content-Type: text/plain\r\n
//	Content-Length: 13\r\n
//	\r\n
//	Hello, world!                              ← body, 13 bytes
//	                                            ← server closes conn
//
// A POST with a body follows the same shape but also includes a
// Content-Length on the request:
//
//	POST /submit HTTP/1.0\r\n
//	Host: example.com\r\n
//	Content-Length: 5\r\n
//	Content-Type: application/x-www-form-urlencoded\r\n
//	\r\n
//	hello
//
// ─── Limitations of HTTP/1.0 ────────────────────────────────────────────────
//
// Each TCP handshake costs ~1 RTT. Opening a fresh TCP connection per
// request is wasteful for any page that loads more than one resource
// (images, scripts, …). This is what HTTP/1.1 solves — see the parallel
// `http1.1/server` package.
//
// ─── What this package does ─────────────────────────────────────────────────
//
// A Server that:
//
//   - accepts connections and spawns a goroutine per connection (HTTP/1.0
//     only handles one request per connection);
//   - parses the request line and header block;
//   - decodes Content-Length into a body reader;
//   - hands the request to the user's handler with a response.Writer that
//     emits the HTTP/1.0 response shape; and
//   - closes the connection.
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

	"github.com/kianooshaz/http-from-scratch/http1/response"
)

// Server is the entry point.
type Server struct {
	Addr    string       // TCP address, e.g. ":9000".
	Handler http.Handler // Defaults to http.DefaultServeMux if nil.
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
				fmt.Printf("http/1.0: connection error: %s\n", err)
			}
		}()
	}
}

// handle services a single connection. HTTP/1.0 serves exactly one request
// per connection, then closes.
func (s *Server) handle(conn net.Conn) error {
	defer conn.Close()

	// ── Bounded header read ────────────────────────────────────────────
	// Headers are an attack surface: a malicious client could send
	// "Header: x\r\n" forever, exhausting memory. We bound the header
	// read to 1 MB with io.LimitReader; after parsing the header block
	// we lift the bound so the body can be any size.
	limitReader := io.LimitReader(conn, 1*1024*1024).(*io.LimitedReader)
	reader := bufio.NewReader(limitReader)
	headerReader := textproto.NewReader(reader)

	req, err := readRequest(headerReader, limitReader, reader, conn)
	if err != nil {
		return err
	}

	// Wrap the conn as an HTTP/1.0 response writer. Note that Proto is
	// pinned to "HTTP/1.0": even if a client claims HTTP/1.1 we will
	// speak 1.0 back, which is exactly the value of this server being
	// separate from the http1.1 package.
	w := &response.Writer{
		Proto:   "HTTP/1.0",
		Conn:    conn,
		Headers: make(http.Header),
	}

	s.Handler.ServeHTTP(w, req)

	// If the handler never wrote a body or status code, emit a default
	// 200 with empty body so the client isn't left hanging waiting for
	// the connection to close.
	if !w.SentHeaders() {
		w.WriteHeader(http.StatusOK)
	}
	return nil
}

// readRequest parses one HTTP/1.0 request from headerReader.
//
// The shape is: request-line, headers, blank line, then Content-Length
// bytes of body. textproto.Reader handles the line-based grammar; we
// feed it through io.LimitReader so a malicious peer can't blow up the
// memory budget by sending headers forever.
func readRequest(headerReader *textproto.Reader, limitReader *io.LimitedReader, reader *bufio.Reader, conn net.Conn) (*http.Request, error) {
	// ── Request line ──────────────────────────────────────────────────
	//   "GET /path HTTP/1.0\r\n"
	reqLine, err := headerReader.ReadLine()
	if err != nil {
		return nil, fmt.Errorf("read request line: %w", err)
	}

	req := new(http.Request)

	method, rest, ok := strings.Cut(reqLine, " ")
	if !ok {
		return nil, errors.New("invalid method")
	}
	if !validMethod(method) {
		return nil, errors.New("invalid method")
	}
	req.Method = method

	requestURI, proto, ok := strings.Cut(rest, " ")
	if !ok {
		return nil, errors.New("invalid request URI")
	}
	if req.URL, err = url.ParseRequestURI(requestURI); err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}
	req.RequestURI = requestURI

	major, minor, ok := parseProtocol(proto)
	if !ok {
		return nil, errors.New("invalid protocol")
	}
	req.Proto = proto
	req.ProtoMajor = major
	req.ProtoMinor = minor

	// ── Header block ──────────────────────────────────────────────────
	// Read Name: Value lines until a blank line. We canonicalize keys
	// to lowercase for consistency with net/http.
	req.Header = make(http.Header)
	for {
		line, err := headerReader.ReadLineBytes()
		if err != nil && err != io.EOF {
			return nil, err
		} else if err != nil {
			break
		}
		if len(line) == 0 {
			break // blank line: end of headers
		}

		k, v, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			return nil, errors.New("invalid header")
		}
		req.Header.Add(strings.ToLower(string(k)), strings.TrimLeft(string(v), " "))
	}

	// Headers are bounded; the body is not. Lift the limit so the body
	// can stream any size we have memory for.
	limitReader.N = math.MaxInt64

	// ── Context plumbing ──────────────────────────────────────────────
	// net/http handlers expect certain context keys (e.g. LocalAddrContextKey
	// for Server.Addr). We populate the minimum useful set.
	ctx := context.Background()
	ctx = context.WithValue(ctx, http.LocalAddrContextKey, conn.LocalAddr())
	req = req.WithContext(ctx)

	// ── Body ──────────────────────────────────────────────────────────
	// HTTP/1.0 has no chunked encoding (that's an HTTP/1.1 feature), so
	// the only way to signal a body length is Content-Length. Missing or
	// zero means "no body".
	contentLength, err := parseContentLength(req.Header.Get("Content-Length"))
	if err != nil {
		return nil, err
	}
	req.ContentLength = contentLength
	if contentLength > 0 {
		req.Body = &bodyReader{reader: io.LimitReader(reader, contentLength)}
	} else {
		req.Body = noBody{}
	}

	req.RemoteAddr = conn.RemoteAddr().String()
	req.Close = true // HTTP/1.0 connections always close.

	return req, nil
}

// parseContentLength returns the integer in headerval, or 0 if absent.
// We deliberately do not return an error for missing Content-Length: that
// is the common case for GET.
func parseContentLength(headerval string) (int64, error) {
	if headerval == "" {
		return 0, nil
	}
	return strconv.ParseInt(headerval, 10, 64)
}

// parseProtocol splits "HTTP/1.0" into major/minor ints.
// We accept HTTP/1.0 (this server's main case) and HTTP/1.1 (because some
// clients always send 1.1 — we'll downgrade them in the response).
func parseProtocol(proto string) (int, int, bool) {
	switch proto {
	case "HTTP/1.0":
		return 1, 0, true
	case "HTTP/1.1":
		return 1, 1, true
	}
	return 0, 0, false
}

// validMethod rejects unknown methods early. The allowed set mirrors
// net/http's MethodGet..MethodTrace constants.
func validMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// ─── Request body helpers ──────────────────────────────────────────────────
//
// A real net/http request body is io.ReadCloser. We need to satisfy that
// shape so handler code that does `defer r.Body.Close()` works.

type bodyReader struct{ reader io.Reader }

func (r *bodyReader) Read(p []byte) (int, error) { return r.reader.Read(p) }

// Close drains any unread body so the connection can be reused or, in
// HTTP/1.0's case, so we don't leak file descriptors. HTTP/1.0 closes the
// connection after this anyway, but draining is good hygiene.
func (r *bodyReader) Close() error {
	_, err := io.Copy(io.Discard, r.reader)
	return err
}

// noBody is the zero-content request body. Returning the zero value would
// also work; the named type makes intent clear.
type noBody struct{}

func (noBody) Read([]byte) (int, error) { return 0, io.EOF }
func (noBody) Close() error             { return nil }
