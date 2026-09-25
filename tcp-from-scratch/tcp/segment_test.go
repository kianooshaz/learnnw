package tcp

import "testing"

// These tests pin Stage 1 to the actual wire format, so that everything we
// build on top (encode/decode in Stage 2, the state machine in Stage 3...)
// shares one correct picture of the header.

func TestFlagBitsMatchRFC(t *testing.T) {
	// The bit values are not our choice: they are fixed by the RFC, because
	// every TCP peer on the internet must agree on which bit is which.
	if FlagFIN != 0x01 || FlagSYN != 0x02 || FlagRST != 0x04 ||
		FlagPSH != 0x08 || FlagACK != 0x10 || FlagURG != 0x20 {
		t.Fatalf("flag bits must match RFC 9293: SYN=%#x ACK=%#x FIN=%#x RST=%#x",
			FlagSYN, FlagACK, FlagFIN, FlagRST)
	}
}

func TestSynAckIsTwoBitsInOneByte(t *testing.T) {
	// The second packet of every TCP handshake is SYN and ACK set in the
	// same segment, i.e. both bits OR-ed into the same byte: 0x02 | 0x10.
	s := &Segment{Flags: FlagSYN | FlagACK}

	if s.Flags != 0x12 {
		t.Fatalf("SYN|ACK must be 0x12 on the wire, got %#x", s.Flags)
	}
	if !s.HasFlag(FlagSYN) || !s.HasFlag(FlagACK) {
		t.Fatal("SYN|ACK must report both SYN and ACK")
	}
	if s.HasFlag(FlagFIN) {
		t.Fatal("SYN|ACK must not contain FIN")
	}
}

func TestHeaderLen(t *testing.T) {
	// Fixed header = five 32-bit words = 20 bytes, the value every plain
	// handshake packet uses for DataOffset.
	if HeaderLen != 20 {
		t.Fatalf("fixed TCP header must be 20 bytes, got %d", HeaderLen)
	}

	s := &Segment{DataOffset: 5}
	if got := s.HeaderBytes(); got != 20 {
		t.Fatalf("DataOffset 5 must be 20 bytes, got %d", got)
	}

	// The theoretical maximum, 15 words, is what bounds the options space
	// at 40 bytes (60 - 20).
	max := (&Segment{DataOffset: 15}).HeaderBytes()
	if max != 60 {
		t.Fatalf("DataOffset 15 must be 60 bytes, got %d", max)
	}
}

func TestString(t *testing.T) {
	// A bare SYN: no ACK field printed, because the ACK bit is off and the
	// Acknowledgment Number field is meaningless without it.
	syn := &Segment{SrcPort: 54321, DstPort: 443, Seq: 1000, Window: 65535, Flags: FlagSYN}
	if got, want := syn.String(), "54321 > 443 seq=1000 win=65535 len=0 [S]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// The SYN/ACK that answers it: ACK flag set, so the ack value appears.
	synack := &Segment{
		SrcPort: 443, DstPort: 54321,
		Seq: 7000, Ack: 1001,
		Window: 65535, Flags: FlagSYN | FlagACK,
	}
	if got, want := synack.String(), "443 > 54321 seq=7000 ack=1001 win=65535 len=0 [SA]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
