# tcp-from-scratch

A simplified TCP stack written from scratch in Go, to learn how TCP really
works. Not production code: every mechanism is hand-rolled and explained.

## Ground rules

- No `net.TCPConn`, `net.ListenTCP`, `net.DialTCP`, or any high-level TCP API.
- No existing TCP libraries. Standard library only, and only for low-level
  plumbing: raw packet I/O, syscalls, checksums, timers, byte order.
- Every TCP mechanism — checksum, handshake, retransmission, flow control,
  congestion control — is implemented here, by hand.
- Code stays small and readable; the explanations live in comments and in the
  accompanying lesson, not in cleverness.

## Roadmap

- [x] Stage 1 — TCP/IP fundamentals: the segment as a data model (`tcp/segment.go`)
- [ ] Stage 2 — Segment encoding/decoding, options, Internet checksum
- [ ] Stage 3 — The TCP state machine (11 states, the important transitions)
- [ ] Stage 4 — The three-way handshake, by hand
- [ ] Stage 5 — Reliable data transfer: seq/ack, retransmission, buffers, out-of-order
- [ ] Stage 6 — Flow control: advertised window, sliding window
- [ ] Stage 7 — Retransmission timers: RTO, RTT measurement, exponential backoff
- [ ] Stage 8 — Connection termination: FIN, four-way close, TIME-WAIT
- [ ] Stage 9 — Congestion control: slow start, congestion avoidance, fast retransmit/recovery
- [ ] Stage 10 — A minimal socket-like API: Listen/Accept/Connect/Read/Write/Close

## Running the tests

```
go test ./...
```

Commit after each stage, so the project's history doubles as the tutorial.
