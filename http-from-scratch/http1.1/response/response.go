// Package response implements the HTTP/1.1 response writer.
//
// ─── Beyond HTTP/1.0 ───────────────────────────────────────────────────────
//
// The status line, header block, and body shape are unchanged from 1.0.
// What differs is:
//
//   - Transfer-Encoding: chunked. If the handler doesn't set Content-Length
//     or Transfer-Encoding itself, we emit responses chunked by default.
//     The chunked encoder adds a length prefix to each Write, so the body
//     can stream without us knowing its size up front.
//
//   - Connection: keep-alive|close. We set this header from req.Close:
//     close if the client asked to close, keep-alive otherwise. Without
//     this, peers on a persistent connection wouldn't know whether to
//     expect more requests.
//
//   - Trailing "0\r\n\r\n". When chunked encoding is on, the response
//     writer must flush a zero-length terminating chunk at the end of
//     each response to signal "body done". That's what Flush() does,
//     and the server's per-request loop calls it after the handler
//     returns.
//
// ─── Why a buffered body? ───────────────────────────────────────────────────
//
// There's a subtle problem: when chunked encoding is on, we want headers
// to be written before the first chunk, but we don't know whether to
// chunk until we see the headers. So:
//
//   - WriteHeader may write headers (good case) OR decide to chunk based
//     on header inspection (still good — we can now emit a status line
//     plus "Transfer-Encoding: chunked" header).
//   - Write (when headers haven't been sent yet) must send a status line,
//     default 200, the headers we've seen, and then the body bytes.
//   - flushBufferedBody() exists to allow Write to enqueue body bytes
//     before the status line by pushing them into a small buffer, then
//     draining that buffer once headers are out.
package response

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/kianooshaz/http-from-scratch/http1.1/chunked"
)

var nlcf = []byte{'\r', '\n'}

// Writer implements http.ResponseWriter for HTTP/1.1.
type Writer struct {
	Req             *http.Request // Current request — used for Proto, Close, etc.
	Conn            net.Conn      // Underlying connection.
	Headers         http.Header   // Headers; populated by handlers via Header().
	SentHeaders     bool          // True once the status line + headers have been written.
	ChunkedEncoding bool          // True if this response uses Transfer-Encoding: chunked.
	BodyBuffer      *bytes.Buffer // Tiny staging buffer used when Write is called before WriteHeader.
}

// Header returns the response headers. Always non-nil because the server
// initializes Headers at construction time.
func (w *Writer) Header() http.Header { return w.Headers }

// WriteHeader sends the status line and headers. A second call is a logged
// warning and ignored, matching net/http.
//
// On first call we also pick a transfer encoding: if the handler hasn't
// specified Content-Length or Transfer-Encoding, default to chunked.
func (w *Writer) WriteHeader(statusCode int) {
	if w.SentHeaders {
		slog.Warn(fmt.Sprintf("WriteHeader called twice, second time with: %d", statusCode))
		return
	}

	// Pick transfer encoding. Precedence matches net/http:
	//   - If Transfer-Encoding: set by handler → honor it.
	//   - If Content-Length:    set by handler → no chunking.
	//   - Otherwise:            emit chunked.
	_, hasCL := w.Headers["Content-Length"]
	_, hasTE := w.Headers["Transfer-Encoding"]
	if !hasCL && !hasTE {
		w.ChunkedEncoding = true
		w.Headers.Set("Transfer-Encoding", "chunked")
	}

	// Tell the peer whether we'll be around for another request.
	if w.Req.Close {
		w.Headers.Set("Connection", "close")
	} else {
		w.Headers.Set("Connection", "keep-alive")
	}

	if err := writeStatusLine(w.Conn, w.Req.Proto, statusCode); err != nil {
		slog.Error("write status line", "err", err)
		return
	}
	if err := writeHeaders(w.Conn, w.Headers); err != nil {
		slog.Error("write headers", "err", err)
		return
	}
	w.SentHeaders = true
	w.flushBufferedBody()
}

// Write sends body bytes, flushing the headers first if necessary.
//
// If the handler forgot to call WriteHeader, we treat Write as 200 OK.
// For untyped bodies we also default Content-Type by sniffing the first
// few bytes (matches net/http's behavior).
func (w *Writer) Write(b []byte) (int, error) {
	if !w.SentHeaders {
		if w.Headers.Get("Content-Type") == "" {
			w.Headers.Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	return w.writeBody(b)
}

// Flush terminates chunked encoding on this response (writing the
// "0\r\n\r\n" trailer) and drains any buffered bytes. The server's
// per-connection loop calls this after each handler returns.
func (w *Writer) Flush() error {
	if w.ChunkedEncoding {
		if _, err := w.Conn.Write([]byte("0\r\n\r\n")); err != nil {
			return err
		}
	}
	w.flushBufferedBody()
	return nil
}

// writeBody routes bytes through the chunked encoder when applicable.
func (w *Writer) writeBody(b []byte) (int, error) {
	if w.ChunkedEncoding {
		enc := chunked.NewEncoder(w.Conn)
		return enc.Write(b)
	}
	return w.Conn.Write(b)
}

func (w *Writer) flushBufferedBody() {
	if w.BodyBuffer != nil {
		if _, err := w.Conn.Write(w.BodyBuffer.Bytes()); err != nil {
			slog.Error("write buffered body", "err", err)
		}
		w.BodyBuffer = nil
	}
}

// writeStatusLine writes "HTTP/1.1 200 OK\r\n".
func writeStatusLine(w io.Writer, proto string, statusCode int) error {
	if _, err := io.WriteString(w, proto); err != nil {
		return err
	}
	if _, err := w.Write([]byte{' '}); err != nil {
		return err
	}
	if _, err := io.WriteString(w, strconv.Itoa(statusCode)); err != nil {
		return err
	}
	if _, err := w.Write([]byte{' '}); err != nil {
		return err
	}
	if _, err := io.WriteString(w, http.StatusText(statusCode)); err != nil {
		return err
	}
	_, err := w.Write(nlcf)
	return err
}

// writeHeaders writes each header as "Name: Value\r\n" followed by a blank
// "\r\n" to terminate the header block.
func writeHeaders(w io.Writer, headers http.Header) error {
	for k, vals := range headers {
		for _, v := range vals {
			if _, err := io.WriteString(w, k); err != nil {
				return err
			}
			if _, err := w.Write([]byte{':', ' '}); err != nil {
				return err
			}
			if _, err := io.WriteString(w, v); err != nil {
				return err
			}
			if _, err := w.Write(nlcf); err != nil {
				return err
			}
		}
	}
	_, err := w.Write(nlcf)
	return err
}
