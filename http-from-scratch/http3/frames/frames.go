// Package frames implements the HTTP/3 frame layer (RFC 9114 §7) and the
// QUIC variable-length integer (RFC 9000 §16) it is built on.
//
// ─── Why frames? ────────────────────────────────────────────────────────────
//
// HTTP/1.x is a line-oriented protocol: "GET / HTTP/1.1\r\n", "Name: v\r\n".
// HTTP/3 is binary. Everything that moves on a QUIC stream is a sequence of
// frames, and every frame is three parts:
//
//	type (varint)   length (varint)   payload (length bytes)
//
// A DATA frame carrying "Hello" on the wire is:
//
//	0x00            0x05              "Hello"
//	^ type: DATA    ^ 5 bytes long    ^ the payload itself
//
// and the status line + headers of a response are not text at all — they are
// a single HEADERS frame whose payload is a QPACK-encoded field section.
//
// ─── The variable-length integer ────────────────────────────────────────────
//
// QUIC and HTTP/3 share one integer encoding (RFC 9000 §16). The two most
// significant bits of the first byte select the total encoding length:
//
//	0b00xxxxxx   1 byte,  values 0 .. 2^6-1
//	0b01xxxxxx   2 bytes, values 0 .. 2^14-1
//	0b10xxxxxx   4 bytes, values 0 .. 2^30-1
//	0b11xxxxxx   8 bytes, values 0 .. 2^62-1
//
// The remaining bits hold the value, big-endian. 63 encodes as the single
// byte 0x3f; 15293 encodes as 0x7b 0xbd (the 0x7b carries the 01 prefix).
package frames

import (
	"bytes"
	"fmt"
	"io"
)

// Frame types (RFC 9114 §7.2).
//
// Types 0x2, 0x6, 0x8, 0x9 and every type of the form 0x1f*N + 0x21 are
// reserved: they must never be sent, and receivers must skip them. See
// reservedType.
const (
	FrameData        uint64 = 0x0 // request/response body bytes
	FrameHeaders     uint64 = 0x1 // QPACK-encoded field section
	FrameCancelPush  uint64 = 0x3 // defined, but we never push
	FrameSettings    uint64 = 0x4 // connection parameters
	FramePushPromise uint64 = 0x5 // defined, but we never push
	FrameGoAway      uint64 = 0x7 // "no more requests on this connection"
	FrameMaxPushID   uint64 = 0xD // defined, but we never push
)

// SETTINGS identifiers (RFC 9114 §7.2.4.1) that this codebase cares about.
const (
	SettingsQPACKMaxTableCapacity uint64 = 0x1
	SettingsMaxFieldSectionSize   uint64 = 0x6
	SettingsQPACKBlockedStreams   uint64 = 0x7
)

// HTTP/3 and QPACK error codes (RFC 9114 §8.1, RFC 9204 §6.3). These ride
// along in QUIC's RESET_STREAM and CONNECTION_CLOSE frames.
const (
	ErrH3NoError                uint64 = 0x100
	ErrH3GeneralProtocolError   uint64 = 0x101
	ErrH3StreamCreationError    uint64 = 0x103
	ErrH3ClosedCriticalStream   uint64 = 0x104
	ErrH3FrameUnexpected        uint64 = 0x105
	ErrH3FrameError             uint64 = 0x106
	ErrH3SettingsError          uint64 = 0x109
	ErrH3MissingSettings        uint64 = 0x10a
	ErrH3RequestRejected        uint64 = 0x10b
	ErrH3RequestCanceled        uint64 = 0x10c
	ErrH3RequestIncomplete      uint64 = 0x10d
	ErrH3MessageError           uint64 = 0x10e
	ErrQPACKDecompressionFailed uint64 = 0x200
)

// reservedType reports whether a frame type is reserved (RFC 9114 §7.2.8).
// Reserved types are never sent by a conforming peer, but a receiver must
// skip them instead of erroring — that's how grease exercises parser
// robustness.
func reservedType(t uint64) bool {
	switch t {
	case 0x2, 0x6, 0x8, 0x9:
		return true
	}
	return t >= 0x21 && (t-0x21)%0x1f == 0
}

// maxFramePayload bounds how much payload we are willing to buffer for one
// frame. The length prefix is attacker-controlled input, so it is never
// trusted as an allocation size; header blocks are tiny and DATA frames in
// practice carry at most one QUIC flow-control window, so 1 MiB is generous.
const maxFramePayload = 1 << 20

// Frame is a single HTTP/3 frame.
type Frame struct {
	Type    uint64
	Payload []byte
}

// ReadVarint reads one QUIC variable-length integer from r (RFC 9000 §16).
func ReadVarint(r io.Reader) (uint64, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return 0, err
	}
	rest := make([]byte, 1<<(first[0]>>6)-1)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, err
	}
	v := uint64(first[0] & 0x3f)
	for _, b := range rest {
		v = v<<8 | uint64(b)
	}
	return v, nil
}

// AppendVarint appends the varint encoding of v to b.
func AppendVarint(b []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(b, byte(v))
	case v < 1<<14:
		return append(b, byte(v>>8)|0x40, byte(v))
	case v < 1<<30:
		return append(b, byte(v>>24)|0x80, byte(v>>16), byte(v>>8), byte(v))
	default:
		return append(b, byte(v>>56)|0xc0, byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
}

// Read reads one frame from r, buffering its payload. io.EOF here means the
// peer sent FIN — on a request stream that is "request (or body) complete".
func Read(r io.Reader) (Frame, error) {
	t, err := ReadVarint(r)
	if err != nil {
		return Frame{}, err
	}
	n, err := ReadVarint(r)
	if err != nil {
		return Frame{}, err
	}
	if n > maxFramePayload {
		return Frame{}, fmt.Errorf("frame type %#x: payload %d exceeds %d bytes", t, n, maxFramePayload)
	}

	// io.CopyN grows the buffer only as bytes actually arrive, so a lying
	// length prefix can't make us allocate up front.
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, r, int64(n)); err != nil {
		return Frame{}, fmt.Errorf("frame type %#x: read payload: %w", t, err)
	}
	return Frame{Type: t, Payload: buf.Bytes()}, nil
}

// Write writes one complete frame.
func Write(w io.Writer, typ uint64, payload []byte) error {
	if err := WriteFrameHeader(w, typ, uint64(len(payload))); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// WriteFrameHeader writes just "type length". Response bodies use it to
// stream DATA frames without buffering the payload.
func WriteFrameHeader(w io.Writer, typ, length uint64) error {
	_, err := w.Write(AppendVarint(AppendVarint(nil, typ), length))
	return err
}

// EncodeSettings renders id/value pairs as a SETTINGS frame payload: a
// sequence of varint identifier + varint value.
func EncodeSettings(pairs ...[2]uint64) []byte {
	var b []byte
	for _, p := range pairs {
		b = AppendVarint(AppendVarint(b, p[0]), p[1])
	}
	return b
}

// ParseSettings parses a SETTINGS payload. Unknown identifiers MUST be
// ignored (RFC 9114 §7.2.4) — that is how new settings get deployed without
// breaking old peers.
func ParseSettings(payload []byte) (map[uint64]uint64, error) {
	settings := make(map[uint64]uint64)
	for len(payload) > 0 {
		id, err := varintFromSlice(&payload)
		if err != nil {
			return nil, err
		}
		v, err := varintFromSlice(&payload)
		if err != nil {
			return nil, err
		}
		settings[id] = v
	}
	return settings, nil
}

// varintFromSlice reads one varint from the front of *p, advancing it.
func varintFromSlice(p *[]byte) (uint64, error) {
	b := *p
	if len(b) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := 1 << (b[0] >> 6)
	if len(b) < n {
		return 0, io.ErrUnexpectedEOF
	}
	v := uint64(b[0] & 0x3f)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
	}
	*p = b[n:]
	return v, nil
}
