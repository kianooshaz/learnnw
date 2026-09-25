// Package response implements the HTTP/3 response writer.
//
// ─── What changed from HTTP/1.1 ─────────────────────────────────────────────
//
// The text status line and header block from HTTP/1.x no longer exist.
// A response is now a sequence of frames on the request's QUIC stream:
//
//	HEADERS frame    ← :status and every header, QPACK-encoded
//	DATA frame(s)    ← body bytes, each frame self-describing its length
//	FIN              ← the QUIC stream's end-of-stream marker
//
// Three HTTP/1.1 problems simply disappear:
//
//   - No Content-Length, no chunked encoding. The FIN tells the peer
//     exactly where the body ends and every DATA frame carries its own
//     length prefix. Body framing — the single biggest source of
//     complexity in 1.0 and 1.1 — became the transport's job.
//   - No Connection: keep-alive. Every request is its own stream on a
//     connection that lives as long as both peers want it to.
//   - No header-buffering tricks to compute Content-Length before the
//     first Write. There is no Content-Length to compute.
//
// ─── QPACK ──────────────────────────────────────────────────────────────────
//
// Header compression in HTTP/3 is QPACK (RFC 9204), a variant of HPACK
// redesigned for out-of-order delivery across streams. Its dynamic table
// creates cross-stream dependencies: stream N's headers may reference
// entries that only become valid once an earlier stream's inserts arrive.
// This codebase opts out entirely — it advertises a max table capacity of
// 0 and encodes with static-table indexes and string literals only — so
// every header block decodes the moment it arrives. That is a conforming
// (if less compressed) subset, and it is what curl etc. fall back to when
// they advertise capacity 0.
package response

import (
	"bytes"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/quic-go/qpack"
	"github.com/quic-go/quic-go"

	"github.com/kianooshaz/http-from-scratch/http3/frames"
)

// Writer implements http.ResponseWriter for HTTP/3, on top of one request
// stream.
type Writer struct {
	Stream      *quic.Stream
	Headers     http.Header
	SentHeaders bool
}

// Header returns the response headers.
func (w *Writer) Header() http.Header { return w.Headers }

// WriteHeader QPACK-encodes ":status" plus the handler's headers and sends
// them as one HEADERS frame. A second call is a logged warning and
// ignored, matching net/http.
func (w *Writer) WriteHeader(statusCode int) {
	if w.SentHeaders {
		slog.Warn("WriteHeader called twice, second time with: " + strconv.Itoa(statusCode))
		return
	}

	var block bytes.Buffer
	enc := qpack.NewEncoder(&block)
	if err := enc.WriteField(qpack.HeaderField{
		Name:  ":status",
		Value: strconv.Itoa(statusCode),
	}); err != nil {
		slog.Error("qpack-encode :status", "err", err)
		return
	}
	for name, vals := range w.Headers {
		// HTTP/3 header names are lowercase on the wire (pseudo-headers
		// must be lowercase, and net/http likewise lowercases before
		// writing HTTP/2 and 3).
		for _, v := range vals {
			if err := enc.WriteField(qpack.HeaderField{
				Name:  strings.ToLower(name),
				Value: v,
			}); err != nil {
				slog.Error("qpack-encode header", "name", name, "err", err)
				return
			}
		}
	}
	if err := enc.Close(); err != nil {
		slog.Error("qpack-close", "err", err)
		return
	}
	if err := frames.Write(w.Stream, frames.FrameHeaders, block.Bytes()); err != nil {
		slog.Error("write HEADERS frame", "err", err)
		return
	}
	w.SentHeaders = true
}

// Write sends body bytes as a DATA frame, emitting headers first if the
// handler forgot WriteHeader. If the handler never called WriteHeader, we
// treat Write as 200 OK and sniff a Content-Type from the first bytes,
// exactly like the earlier versions and net/http.
func (w *Writer) Write(b []byte) (int, error) {
	if !w.SentHeaders {
		if w.Headers.Get("Content-Type") == "" {
			w.Headers.Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	// Write only the frame header, then the payload straight through:
	// large bodies become many DATA frames without extra buffering.
	if err := frames.WriteFrameHeader(w.Stream, frames.FrameData, uint64(len(b))); err != nil {
		return 0, err
	}
	return w.Stream.Write(b)
}
