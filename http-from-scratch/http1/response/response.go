// Package response implements the HTTP/1.0 response writer.
//
// ─── Anatomy of an HTTP/1.0 response ─────────────────────────────────────────
//
// Status line:
//
//	HTTP/1.0 200 OK\r\n
//
// Header block (one or more):
//
//	Content-Type: text/plain\r\n
//	Content-Length: 13\r\n
//
// Blank line separator:
//
//	\r\n
//
// Body (Content-Length bytes):
//
//	Hello, world!
//
// The complication: Go's http.ResponseWriter contract requires that
// WriteHeader and Write can be called in either order, and that the
// status line + headers are written before the first body byte. We
// therefore buffer headers until either WriteHeader or Write is called,
// then flush them in the right shape.
//
// This is what every real HTTP framework has to do. net/http does
// exactly the same trick.
package response

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
)

// Writer implements http.ResponseWriter for HTTP/1.0.
//
// Its fields are exported so the server package can construct it in one
// literal — typical of small library types where the cost of constructors
// outweighs the cost of a wider API.
type Writer struct {
	Proto       string      // "HTTP/1.0" by convention; the version string we put on the wire.
	Conn        net.Conn    // Underlying connection.
	Headers     http.Header // Headers; nil-safe by construction (NewWriter-in-the-server initializes this).
	sentHeaders bool        // Internal flag — exported via SentHeaders().
}

// Header returns the response headers. It is always non-nil because
// the server initializes Headers at construction time.
func (w *Writer) Header() http.Header { return w.Headers }

// SentHeaders reports whether WriteHeader (or a Write that triggered an
// implicit 200) has already flushed the status line and headers. The
// server uses this to decide whether to emit a default 200 OK at the end
// of a handler.
func (w *Writer) SentHeaders() bool { return w.sentHeaders }

// WriteHeader writes the status line and headers to the connection. A
// second call is logged and ignored, matching net/http.
func (w *Writer) WriteHeader(statusCode int) {
	if w.sentHeaders {
		slog.Warn(fmt.Sprintf("WriteHeader called twice, second time with: %d", statusCode))
		return
	}
	w.sentHeaders = true

	// We swallow errors here. Why? Once we've started writing the
	// response, there's nothing useful the caller can do with an
	// error from WriteHeader. The next Write will surface the
	// underlying conn failure.
	if _, err := io.WriteString(w.Conn, w.Proto+" "); err != nil {
		return
	}
	if _, err := io.WriteString(w.Conn, strconv.Itoa(statusCode)+" "); err != nil {
		return
	}
	if _, err := io.WriteString(w.Conn, http.StatusText(statusCode)+"\r\n"); err != nil {
		return
	}
	for k, vals := range w.Headers {
		for _, v := range vals {
			if _, err := io.WriteString(w.Conn, k+": "+v+"\r\n"); err != nil {
				return
			}
		}
	}
	if _, err := io.WriteString(w.Conn, "\r\n"); err != nil {
		return
	}
}

// Write sends body bytes, flushing the headers first if needed.
//
// If the handler forgets to call WriteHeader, we treat that as a 200.
// This matches net/http and lets short handlers like
//
//	http.Error(w, "teapot", http.StatusTeapot)
//
// just work — http.Error internally calls WriteHeader then Write.
func (w *Writer) Write(b []byte) (int, error) {
	if !w.sentHeaders {
		w.WriteHeader(http.StatusOK)
	}
	return w.Conn.Write(b)
}
