// Package server implements an HTTP/3 server.
//
// ─── What HTTP/3 changes ────────────────────────────────────────────────────
//
// HTTP/3 (RFC 9114) keeps the semantics of HTTP/1.1 — methods, status
// codes, headers, bodies — and replaces everything underneath them. TCP is
// gone; in its place is QUIC (RFC 9000): UDP datagrams, TLS 1.3 built into
// the handshake, and *streams* as the unit of concurrency.
//
// One QUIC connection carries many streams. Each HTTP request is one
// client-initiated bidirectional stream:
//
//	client                                server
//	  │ ─── QUIC connect (UDP+TLS) ───►     │
//	  │ ══ stream 0 (bidi) ══════════►      │
//	  │    HEADERS {:method GET, ...}       │
//	  │ ◄═══════════════════════════        │
//	  │    HEADERS {:status 200, ...}       │
//	  │    DATA ...                         │
//	  │ ◄══ FIN ════════════════════        │
//	  │ ══ stream 4 (bidi) ══════════►      │   ← next request, never
//	  │                                     │      blocked behind stream 0
//
// Alongside request streams, both peers open one unidirectional *control
// stream*, and SETTINGS must be the first frame on it:
//
//	unidir → 0x00 (control) + SETTINGS { qpack max table capacity: 0, ... }
//	unidir ← 0x00 (control) + SETTINGS { ... }
//
// ─── What this package hand-writes vs. what it borrows ─────────────────────
//
// A from-scratch QUIC would also mean writing TLS 1.3 and the QUIC packet
// protection layer — a project of its own. So unlike the TCP versions in
// this repo, the transport here is the quic-go library standing in for
// net.Listen/net.Conn. What this package implements by hand is the HTTP/3
// layer on top (RFC 9114): the stream state machine, control streams,
// SETTINGS exchange, frame dispatch, and the mapping of requests and
// responses onto streams. Header compression (QPACK, RFC 9204) comes from
// quic-go/qpack, with the dynamic table disabled in both directions so
// header blocks stay independent of stream order — see the response package.
//
// The payoff of the new transport is that the ugliest parts of HTTP/1.1
// disappear. There is no Content-Length to compute and no chunked encoding,
// because the QUIC stream itself frames the body: every DATA frame carries
// its own length, and FIN ends it. There is no keep-alive negotiation,
// because a connection that carries many streams is the default state.
package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/quic-go/qpack"
	"github.com/quic-go/quic-go"

	"github.com/kianooshaz/http-from-scratch/http3/frames"
	"github.com/kianooshaz/http-from-scratch/http3/response"
)

// Client-initiated unidirectional stream types (RFC 9114 §6.2). The first
// varint on any unidirectional stream says what the stream is for.
const (
	streamTypeControl      uint64 = 0x0
	streamTypePush         uint64 = 0x1 // server-initiated only; clients must not send it
	streamTypeQPACKEncoder uint64 = 0x2
	streamTypeQPACKDecoder uint64 = 0x3
)

// Server is the entry point. QUIC is TLS end to end, so unlike the TCP
// versions there is no plaintext mode: TLSConf must carry at least one
// certificate.
type Server struct {
	Addr    string // UDP address to listen on, e.g. ":4433".
	Handler http.Handler
	TLSConf *tls.Config // "h3" is appended to NextProtos if missing.
}

// ListenAndServe blocks accepting QUIC connections. Each connection runs
// on its own goroutine; each request gets a stream, and each stream gets
// its own goroutine too — requests on one connection never queue behind
// each other.
func (s *Server) ListenAndServe() error {
	if s.Handler == nil {
		s.Handler = http.DefaultServeMux
	}
	tlsConf := s.TLSConf.Clone()
	hasH3 := false
	for _, p := range tlsConf.NextProtos {
		if p == "h3" {
			hasH3 = true
		}
	}
	if !hasH3 {
		tlsConf.NextProtos = append(tlsConf.NextProtos, "h3")
	}

	l, err := quic.ListenAddr(s.Addr, tlsConf, nil)
	if err != nil {
		return err
	}
	defer l.Close()

	for {
		qconn, err := l.Accept(context.Background())
		if err != nil {
			if errors.Is(err, quic.ErrServerClosed) {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.handleConn(qconn)
	}
}

// handleConn services one QUIC connection: it opens the server's control
// stream, then runs two accept loops for the life of the connection —
// bidirectional streams carry one HTTP request each (client-initiated IDs
// 0, 4, 8, …), and unidirectional streams carry control and QPACK data.
func (s *Server) handleConn(qconn *quic.Conn) {
	defer qconn.CloseWithError(quic.ApplicationErrorCode(frames.ErrH3NoError), "")

	go sendControlStream(qconn)

	// One QPACK decoder per connection: header blocks are connection-level
	// data. With the dynamic table disabled it holds no state, so requests
	// can share it freely.
	dec := qpack.NewDecoder()

	go func() {
		for {
			stream, err := qconn.AcceptUniStream(context.Background())
			if err != nil {
				return // connection closed by either side
			}
			go handleUniStream(qconn, stream)
		}
	}()

	for {
		stream, err := qconn.AcceptStream(context.Background())
		if err != nil {
			return // connection closed by either side
		}
		go s.serveRequest(qconn, stream, dec)
	}
}

// sendControlStream opens the server-side control stream and writes the
// two things the spec requires: the stream-type varint, then a SETTINGS
// frame as the first frame (RFC 9114 §7.2.4). The stream stays open for
// the connection's lifetime — closing a control stream is the
// H3_CLOSED_CRITICAL_STREAM error.
//
// We advertise a QPACK dynamic table capacity of 0 and zero blocked
// streams: the encoder on the other side must then use only static-table
// indexes and literals, which keeps every header block independently
// decodable.
func sendControlStream(qconn *quic.Conn) {
	cs, err := qconn.OpenUniStream()
	if err != nil {
		return
	}
	// First byte: stream type 0x00 = control.
	if _, err := cs.Write(frames.AppendVarint(nil, streamTypeControl)); err != nil {
		return
	}
	settings := frames.EncodeSettings(
		[2]uint64{frames.SettingsQPACKMaxTableCapacity, 0},
		[2]uint64{frames.SettingsQPACKBlockedStreams, 0},
	)
	if err := frames.Write(cs, frames.FrameSettings, settings); err != nil {
		return
	}

	// GOAWAY would be written here at shutdown. Until then, just ride
	// along until the connection dies.
	<-cs.Context().Done()
}

// handleUniStream classifies one client-initiated unidirectional stream by
// its first varint and services it accordingly.
func handleUniStream(qconn *quic.Conn, stream *quic.ReceiveStream) {
	typ, err := frames.ReadVarint(stream)
	if err != nil {
		return
	}
	switch typ {
	case streamTypeControl:
		handleControlStream(qconn, stream)
	case streamTypeQPACKEncoder, streamTypeQPACKDecoder:
		// QPACK encoder/decoder streams exist to carry dynamic-table
		// inserts. Both directions here run with the dynamic table
		// disabled, so a conforming peer has nothing required to say on
		// them — drain whatever arrives.
		io.Copy(io.Discard, stream)
	case streamTypePush:
		// Only the server may open push streams; this is H3_STREAM_CREATION_ERROR.
		qconn.CloseWithError(quic.ApplicationErrorCode(frames.ErrH3StreamCreationError),
			"client opened a push stream")
	default:
		// Unknown stream types must be ignored, not rejected (RFC 9114
		// §6.2.1) — that is how the protocol grows without version bumps.
		io.Copy(io.Discard, stream)
	}
}

// handleControlStream reads the peer's control stream: SETTINGS first —
// its absence is H3_MISSING_SETTINGS — then connection-level frames for
// the rest of the connection.
func handleControlStream(qconn *quic.Conn, stream *quic.ReceiveStream) {
	f, err := frames.Read(stream)
	if err != nil {
		return
	}
	if f.Type != frames.FrameSettings {
		qconn.CloseWithError(quic.ApplicationErrorCode(frames.ErrH3MissingSettings),
			"first frame on control stream was not SETTINGS")
		return
	}
	settings, err := frames.ParseSettings(f.Payload)
	if err != nil {
		qconn.CloseWithError(quic.ApplicationErrorCode(frames.ErrH3SettingsError),
			"malformed SETTINGS")
		return
	}
	// The peer's QPACK limits only matter if we used the dynamic table,
	// which our encoder never does — so they're read and ignored here.
	slog.Debug("http/3: peer SETTINGS",
		"qpack_max_table_capacity", settings[frames.SettingsQPACKMaxTableCapacity],
		"qpack_blocked_streams", settings[frames.SettingsQPACKBlockedStreams])

	for {
		f, err := frames.Read(stream)
		if err != nil {
			return
		}
		if f.Type == frames.FrameGoAway {
			return // peer will send no more requests; stop reading
		}
		// CANCEL_PUSH, MAX_PUSH_ID, reserved types: nothing to do, we
		// never push. frames.Read has already consumed the payload.
	}
}

// serveRequest handles one request stream: read the HEADERS frame, build
// an *http.Request, dispatch to the handler, and close the stream — the
// FIN is the response terminator.
func (s *Server) serveRequest(qconn *quic.Conn, stream *quic.Stream, dec *qpack.Decoder) {
	reject := func(code uint64, reason string) {
		stream.CancelRead(quic.StreamErrorCode(code))
		stream.CancelWrite(quic.StreamErrorCode(code))
	}

	// The first frame on a request stream MUST be HEADERS (RFC 9114 §4.1).
	f, err := frames.Read(stream)
	if err != nil {
		reject(frames.ErrH3RequestIncomplete, "reading request HEADERS")
		return
	}
	if f.Type != frames.FrameHeaders {
		reject(frames.ErrH3FrameUnexpected, "first frame on request stream was not HEADERS")
		return
	}

	fields, err := decodeFields(dec, f.Payload)
	if err != nil {
		reject(frames.ErrQPACKDecompressionFailed, "decoding request headers")
		return
	}
	req, err := requestFromFields(qconn, stream, fields)
	if err != nil {
		reject(frames.ErrH3MessageError, err.Error())
		return
	}

	w := &response.Writer{
		Stream:  stream,
		Headers: make(http.Header),
	}
	s.Handler.ServeHTTP(w, req)

	// A handler that never wrote still owes the client a response — same
	// default-200 convention as the HTTP/1.0 server.
	if !w.SentHeaders {
		w.WriteHeader(http.StatusOK)
	}
	// Closing the send side sends FIN: the replacement for Content-Length,
	// chunked termination, and "connection closed" all at once.
	stream.Close()

	// If the handler never drained an upload body, tell the client to stop
	// sending: we answered without it.
	if req.Body != nil {
		req.Body.Close()
	}
}

// decodeFields runs a QPACK-encoded field section through the decoder.
func decodeFields(dec *qpack.Decoder, payload []byte) ([]qpack.HeaderField, error) {
	var fields []qpack.HeaderField
	next := dec.Decode(payload)
	for {
		hf, err := next()
		if errors.Is(err, io.EOF) {
			return fields, nil
		}
		if err != nil {
			return nil, err
		}
		fields = append(fields, hf)
	}
}

// requestFromFields maps a decoded field section onto an *http.Request.
// The pseudo-headers (:method, :scheme, :authority, :path) play the role
// HTTP/1.1 gave to the request line; everything else is an ordinary header.
func requestFromFields(qconn *quic.Conn, stream *quic.Stream, fields []qpack.HeaderField) (*http.Request, error) {
	var method, scheme, authority, path string
	header := make(http.Header)
	for _, f := range fields {
		if strings.HasPrefix(f.Name, ":") {
			switch f.Name {
			case ":method":
				method = f.Value
			case ":scheme":
				scheme = f.Value
			case ":authority":
				authority = f.Value
			case ":path":
				path = f.Value
			default:
				return nil, fmt.Errorf("unknown pseudo-header %q", f.Name)
			}
			continue
		}
		// Field names arrive lowercase on the wire; store them canonically
		// like net/http does, so handler code (Header.Get etc.) works.
		header.Add(http.CanonicalHeaderKey(f.Name), f.Value)
	}
	if method == "" || scheme == "" || path == "" {
		return nil, errors.New("request missing required pseudo-headers")
	}
	u, err := url.ParseRequestURI(path)
	if err != nil {
		return nil, fmt.Errorf("invalid :path %q: %w", path, err)
	}
	u.Scheme = scheme
	u.Host = authority

	req := &http.Request{
		Method:     method,
		URL:        u,
		Proto:      "HTTP/3",
		ProtoMajor: 3,
		ProtoMinor: 0,
		Header:     header,
		Host:       authority,
		RequestURI: path,
		// Bodies are framed by DATA frames and the stream FIN, so their
		// length is simply unknown up front. -1 is net/http's "unknown".
		ContentLength: -1,
		Body:          &frameBody{stream: stream},
		RemoteAddr:    qconn.RemoteAddr().String(),
	}
	ctx := context.WithValue(context.Background(), http.LocalAddrContextKey, qconn.LocalAddr())
	return req.WithContext(ctx), nil
}

// frameBody is a request body: whatever DATA frames follow the HEADERS
// frame, until trailers, FIN, or cancel. There is no Content-Length — the
// stream itself is the length.
type frameBody struct {
	stream *quic.Stream
	buf    bytes.Buffer
}

func (b *frameBody) Read(p []byte) (int, error) {
	for b.buf.Len() == 0 {
		f, err := frames.Read(b.stream)
		if err != nil {
			return 0, err // io.EOF (clean FIN) or a transport error
		}
		switch f.Type {
		case frames.FrameData:
			b.buf.Write(f.Payload)
		case frames.FrameHeaders:
			// A second HEADERS frame is trailers; the body is over (we
			// discard trailers in this codebase).
			return 0, io.EOF
		default:
			// Reserved or unknown frame types: skip.
		}
	}
	return b.buf.Read(p)
}

// Close stops caring about the rest of an upload body, releasing the
// client's flow-control obligation to us.
func (b *frameBody) Close() error {
	b.stream.CancelRead(quic.StreamErrorCode(frames.ErrH3RequestCanceled))
	return nil
}
