// Package tcp implements a minimal, educational TCP stack from scratch.
//
// The goal is understanding, not production use: every mechanism (checksum,
// handshake, retransmission, flow control, congestion control) is implemented
// by hand and explained in the commit history, one stage at a time.
//
// Stage 1: the data model of a TCP segment — what actually sits on the wire.
package tcp

import "fmt"

// Flag bits in the header's 8-bit flags field (byte at offset 13).
// Each is a single bit; one segment may combine several — the second packet
// of the handshake is SYN|ACK = 0x12 on the wire.
//
// Order within the byte, from the least significant bit up
// (RFC 9293, section 3.1):
//
//	bit 0  FIN  0x01  "no more data from me" — starts connection close
//	bit 1  SYN  0x02  synchronize sequence numbers — starts a connection
//	bit 2  RST  0x04  abort the connection
//	bit 3  PSH  0x08  push: deliver this to the application promptly
//	bit 4  ACK  0x10  the Acknowledgment Number field is meaningful
//	bit 5  URG  0x20  the Urgent Pointer field is meaningful (rarely used)
//	bit 6  ECE  0x40  ECN-Echo (congestion notification; later stage)
//	bit 7  CWR  0x80  Congestion Window Reduced (later stage)
const (
	FlagFIN = 0b0000_0001
	FlagSYN = 0b0000_0010
	FlagRST = 0b0000_0100
	FlagPSH = 0b0000_1000
	FlagACK = 0b0001_0000
	FlagURG = 0b0010_0000
	FlagECE = 0b0100_0000
	FlagCWR = 0b1000_0000
)

// HeaderLen is the size of the fixed part of the TCP header: 20 bytes,
// i.e. five 32-bit words. Options live between byte 20 and 60.
const HeaderLen = 20

// Segment is the in-memory model of one TCP segment: a header plus payload.
//
// Every later stage (state machine, retransmission, flow control) reads and
// writes this struct; the translation to and from raw wire bytes is Stage 2.
// Fields are stored as plain host-order integers here — network byte order
// matters only when we serialize.
type Segment struct {
	SrcPort uint16 // port of the sender
	DstPort uint16 // port of the receiver

	// Seq is the sequence number of the segment's first payload byte.
	// TCP numbers *bytes*, not packets: a 100-byte segment starting at
	// Seq=1000 makes the peer expect 1100 next.
	Seq uint32

	// Ack is the next byte in the opposite direction the sender of this
	// segment is waiting for ("I have everything below N"). Cumulative:
	// one Ack implicitly confirms every earlier byte. Only meaningful
	// when the ACK flag is set.
	Ack uint32

	// DataOffset is the header length measured in 32-bit words (4 bytes),
	// as the RFC defines it. 5 means 20 bytes: fixed header, no options.
	// Maximum 15 means 60 bytes.
	DataOffset uint8

	// Flags is the OR of the Flag* bit constants above.
	Flags uint8

	// Window is the receive window this segment advertises to the other
	// side: "you may have at most this many unacknowledged bytes in
	// flight toward me". This field IS flow control (Stage 6).
	Window uint16

	// Checksum covers a pseudo-header (IP addresses + protocol + length),
	// this header, and the payload. Set to 0 before computing.
	// Implemented in Stage 2.
	Checksum uint16

	// Urgent is the urgent pointer, used only with the URG flag.
	// Kept for completeness; TCP apps almost never use it.
	Urgent uint16

	// Options is the raw options block (MSS, window scale, timestamps...).
	// Must be padded to a multiple of 4 bytes. Parsed in Stage 2+.
	Options []byte

	// Payload is the application data this segment carries (may be empty:
	// SYN, ACK, and FIN segments usually carry no data).
	Payload []byte
}

// HasFlag reports whether s.Flags contains the given flag bit.
//
// Problem it solves: every state-machine decision ("is this a SYN?",
// "is the ACK bit set?") is a bit test against this one byte.
// Reads: s.Flags. Modifies: nothing. TCP rule: flags are independent
// bits that may be combined in any way.
func (s *Segment) HasFlag(f uint8) bool { return s.Flags&f != 0 }

// HeaderBytes converts DataOffset from its on-the-wire unit (32-bit words)
// to bytes. The receiver needs this to know where the payload begins.
//
// Reads: s.DataOffset. Modifies: nothing.
// TCP rule: header length is measured in words so the 4-bit field can
// express up to 60 bytes.
func (s *Segment) HeaderBytes() int { return int(s.DataOffset) * 4 }

// String renders the segment in the spirit of tcpdump, so output from our
// own stack can be compared line by line against real tcpdump captures.
//
// Reads: the whole header (cheap view). Modifies: nothing.
func (s *Segment) String() string {
	var flags []byte
	for _, f := range []struct {
		bit uint8
		ch  byte
	}{
		{FlagSYN, 'S'}, {FlagFIN, 'F'}, {FlagRST, 'R'},
		{FlagPSH, 'P'}, {FlagACK, 'A'}, {FlagURG, 'U'},
	} {
		if s.HasFlag(f.bit) {
			flags = append(flags, f.ch)
		}
	}
	if len(flags) == 0 {
		flags = []byte{'.'}
	}

	ack := ""
	if s.HasFlag(FlagACK) {
		ack = fmt.Sprintf(" ack=%d", s.Ack)
	}

	return fmt.Sprintf("%d > %d seq=%d%s win=%d len=%d [%s]",
		s.SrcPort, s.DstPort, s.Seq, ack, s.Window, len(s.Payload), flags)
}
