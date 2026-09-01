// Package chunked implements HTTP/1.1 chunked transfer encoding, for both
// reading request bodies (Body) and writing response bodies (Encoder).
//
// ─── Why chunked? ───────────────────────────────────────────────────────────
//
// HTTP/1.0 used Content-Length to tell the client when the body ended.
// That works when the server knows the body size in advance — e.g. a file.
// But for streaming responses (chat messages, live logs, dynamically
// generated content) the length is unknown until the last byte is written.
//
// HTTP/1.1's answer: chunked encoding. Instead of a Content-Length header,
// the body is a sequence of length-prefixed chunks:
//
//	5\r\n
//	hello\r\n
//	6\r\n
//	 world\r\n
//	0\r\n
//	\r\n                ← terminating chunk + trailer section separator
//
// Each chunk is "<size in hex>\r\n<size bytes>\r\n". The final chunk is
// "0\r\n\r\n". Anything after the 0 line up to the next blank line is
// "trailer" headers — we accept them but discard them.
//
// ─── Reading chunked request bodies ─────────────────────────────────────────
//
// `Body` implements io.ReadCloser. It owns no buffer of its own; it parses
// from a *bufio.Reader one chunk at a time. Reads shorter than a chunk
// return whatever is available; reads longer than a chunk return only
// the remaining bytes in the current chunk.
//
// ─── Writing chunked response bodies ────────────────────────────────────────
//
// `Encoder` implements io.Writer. Each call to Write emits one chunk:
// size-prefix, body, trailing CRLF. When you're done writing call Close
// to emit the terminating 0-length chunk.
package chunked

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// crlf is the chunk delimiter used on the wire.
var crlf = []byte{'\r', '\n'}

// ─── Reading ────────────────────────────────────────────────────────────────

// Body is an io.ReadCloser over an HTTP/1.1 chunked request body.
//
// Concurrency: Body is NOT safe for concurrent Read calls. The
// persistent-connection loop in the server package reads one Body at a
// time, which matches net/http.
type Body struct {
	Reader         *bufio.Reader // Underlying reader; usually a bufio over the TCP conn.
	bytesRemaining int64         // Bytes left in the current chunk.
	stickyErr      error         // First error seen; subsequent reads return this verbatim.
}

// Read parses the next chunk (if needed) and returns body bytes.
//
// The bufio.Reader is shared with the header parser; that's fine because
// we only call Read on it after the headers are drained.
func (b *Body) Read(p []byte) (int, error) {
	if b.stickyErr != nil {
		return 0, b.stickyErr
	}

	// Need a new chunk? Read its size in hex from the wire.
	if b.bytesRemaining == 0 {
		size, err := b.nextChunkSize()
		if err != nil {
			b.stickyErr = err
			return 0, err
		}
		b.bytesRemaining = size
	}

	// 0-size chunk = end of body.
	if b.bytesRemaining == 0 {
		return 0, io.EOF
	}

	// Limit the read to the chunk's remaining bytes so we never bleed
	// into the next chunk's size line in the same Read call.
	if int64(len(p)) > b.bytesRemaining {
		p = p[:b.bytesRemaining]
	}
	n, err := b.Reader.Read(p)
	b.bytesRemaining -= int64(n)

	// After consuming a whole chunk, the next bytes on the wire are its
	// terminating CRLF; consume them now so the *next* Read is positioned
	// at the next chunk's size line.
	if b.bytesRemaining == 0 && err == nil {
		if err := b.consumeCRLF(); err != nil {
			b.stickyErr = err
			return n, err
		}
	}

	b.stickyErr = err
	return n, err
}

// Close drains any unread body so the connection can be reused. The
// connection has to be readable all the way to the terminating "0\r\n\r\n"
// before the next request starts.
func (b *Body) Close() error {
	if b.stickyErr == io.EOF {
		return nil
	}
	_, err := io.Copy(io.Discard, b)
	return err
}

// nextChunkSize reads "<size-hex>\r\n" and returns the size. If the size
// is 0, it also consumes any trailer headers up to the blank line.
func (b *Body) nextChunkSize() (int64, error) {
	line, err := b.readLine()
	if err != nil {
		return 0, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
	if err != nil {
		return 0, err
	}
	if size == 0 {
		// Discard trailer headers. A real implementation would parse
		// them and surface them via Trailer; for the learning version
		// we just skip them.
		for {
			trailer, err := b.readLine()
			if err != nil {
				return 0, err
			}
			if trailer == "" {
				break
			}
		}
	}
	return size, nil
}

// readLine is a small helper that reads one CRLF-terminated line from the
// bufio.Reader, stripping the trailing \r and the final \n.
func (b *Body) readLine() (string, error) {
	var line []byte
	for {
		c, err := b.Reader.ReadByte()
		if err != nil {
			return "", err
		}
		if c == '\n' {
			break
		}
		line = append(line, c)
	}
	return strings.TrimRight(string(line), "\r"), nil
}

// consumeCRLF reads the two bytes \r\n that come immediately after a
// chunk's body, before the next chunk's size line.
func (b *Body) consumeCRLF() error {
	if c, err := b.Reader.ReadByte(); err != nil || c != '\r' {
		if err != nil {
			return err
		}
		return errors.New("missing CR after chunk")
	}
	if c, err := b.Reader.ReadByte(); err != nil || c != '\n' {
		if err != nil {
			return err
		}
		return errors.New("missing LF after chunk")
	}
	return nil
}

// ─── Writing ────────────────────────────────────────────────────────────────

// Encoder wraps an io.Writer and emits chunked-encoded output. The caller
// should call Close to write the terminating "0\r\n\r\n" chunk.
type Encoder struct {
	w io.Writer
}

// NewEncoder returns an Encoder that writes to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Write emits one chunk for the bytes in p. The size prefix and trailing
// CRLF are added on the wire; the caller only supplies the body bytes.
//
// Returning len(p) (even if the conn.Write at the bottom returned a short
// error — which would have stopped things anyway) keeps io.Writer
// semantics simple: callers expect n == len(p) on success.
func (e *Encoder) Write(p []byte) (int, error) {
	if _, err := fmt.Fprintf(e.w, "%x\r\n", len(p)); err != nil {
		return 0, err
	}
	if _, err := e.w.Write(p); err != nil {
		return 0, err
	}
	if _, err := e.w.Write(crlf); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close writes the terminating zero-length chunk. After Close the
// underlying writer can be reused for a new response.
func (e *Encoder) Close() error {
	_, err := e.w.Write([]byte("0\r\n\r\n"))
	return err
}
