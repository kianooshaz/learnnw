// Package rawclient talks gRPC over a hand-written HTTP/2 client.
//
// ─── Why does this package exist? ───────────────────────────────────────────
//
// gRPC is presented to users as a typed RPC library:
//
//	resp, err := client.Greet(ctx, &proto.GreetRequest{Name: "world"})
//
// That's a few hundred thousand lines of code spread across grpc-go,
// grpc-proto, http2, protobuf, and the Go runtime. None of which tells
// you what's actually happening on the wire.
//
// In this file we strip gRPC down to its essentials:
//
//  1. Open an HTTP/2 connection.
//  2. Encode a request as a length-prefixed protobuf frame.
//  3. Send it with the gRPC wire headers (content-type,
//     :authority, :path, te: trailers, grpc-encoding, grpc-timeout).
//  4. Read a response frame.
//  5. Strip the gRPC status from the trailers.
//
// After reading this file you should be able to explain what the
// generated client in proto/greeter_grpc.pb.go is actually doing on the
// network.
//
// ─── The gRPC wire format ───────────────────────────────────────────────────
//
// A unary RPC body on the wire is just HTTP/2 with a few extra
// conventions:
//
//	POST /greet.v1.GreetService/Greet HTTP/2
//	content-type: application/grpc+proto
//	te: trailers
//	:authority: localhost:9000
//	:path: /greet.v1.GreetService/Greet
//	:scheme: http
//	grpc-encoding: identity
//	grpc-accept-encoding: identity,gzip
//
//	<5-byte gRPC frame header> <protobuf payload>
//
// The 5-byte gRPC frame header is:
//
//	byte 0      : 1 means "compressed" with bit cleared (i.e. NOT
//	              compressed) — see gRPC spec, it's confusing.
//	bytes 1..4  : big-endian uint32 length of the protobuf payload.
//
// Response frames look the same. After the data frames, the server
// sends a "trailers" frame containing grpc-status and grpc-message:
// 0 means OK; non-zero is a gRPC error code.
//
// For more, see:
// https://github.com/grpc/grpc/blob/master/doc/PROTOCOL-HTTP2.md
package rawclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http2"
	"time"

	"google.golang.org/protobuf/proto"

	greetv1 "github.com/kianooshaz/grpc-from-scratch/proto"
)

// grpcContentType is the magic content-type that identifies an HTTP/2
// request as a gRPC call. Real gRPC uses "+proto", "+json", or "+web"
// depending on the wire encoding; we hardcode "+proto" here for clarity.
const grpcContentType = "application/grpc+proto"

// Client is a minimal gRPC client. One instance per host.
//
// Internally it uses net/http with HTTP/2 transport so we get all the
// HTTP/2 framing for free — we just have to format the gRPC-specific
// bits correctly.
type Client struct {
	addr   string // host:port
	client *http.Client
}

// New returns a Client that talks gRPC over HTTP/2 (h2c, no TLS).
func New(addr string) *Client {
	t := &http2.Transport{
		// For h2c (HTTP/2 cleartext) we override DialTLSContext to dial
		// plaintext. The transport still expects this signature; we just
		// ignore the *http2.TLSConfig argument.
		DialTLSContext: func(ctx context.Context, network, addr string, _ *http2.TLSConfig) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		AllowHTTP: true,
	}
	return &Client{
		addr:   addr,
		client: &http.Client{Transport: t},
	}
}

// Greet is the unary RPC client method. It mirrors the generated
// GreetServiceClient.Greet signature so it's easy to compare against
// the auto-generated version in greeter_grpc.pb.go.
func (c *Client) Greet(ctx context.Context, req *greetv1.GreetRequest) (*greetv1.GreetResponse, error) {
	// ─── 1. Encode the request body ───────────────────────────────────
	//
	// protobuf is a binary format. proto.Marshal produces the wire
	// representation directly. There's no JSON, no field names — just
	// field number + value, length-prefixed for varint types.
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// ─── 2. Wrap in a gRPC length-prefixed frame ──────────────────────
	//
	// The 5-byte header is documented in PROTO-HTTP2.md. In practice
	// almost every modern client uses uncompressed protobuf frames,
	// which means byte 0 = 0 (NOT compressed) and bytes 1..4 = the
	// payload length in big-endian uint32.
	frame := make([]byte, 5+len(payload))
	frame[0] = 0 // 0 = identity (uncompressed)
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	// ─── 3. Build the HTTP/2 request ───────────────────────────────────
	//
	// These headers are the gRPC "wire format headers" that identify
	// the request as gRPC and give it a method. Most are standard
	// HTTP/2 pseudo-headers (:authority, :path, :scheme, :method).
	httpReq, err := http.NewRequestWithContext(ctx,
		http.MethodPost,
		"http://"+c.addr+greetv1.GreetService_Greet_FullMethodName,
		bytes.NewReader(frame),
	)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", grpcContentType)
	// "te: trailers" tells the server we're prepared to read response
	// trailers — gRPC uses trailers for grpc-status and grpc-message.
	httpReq.Header.Set("Te", "trailers")
	httpReq.Header.Set("Grpc-Encoding", "identity")
	httpReq.Header.Set("Grpc-Accept-Encoding", "identity")
	// Optional: enforce a server-side deadline via grpc-timeout. Many
	// real clients set this so the server doesn't work forever.
	if dl, ok := ctx.Deadline(); ok {
		timeout := time.Until(dl)
		if timeout > 0 {
			httpReq.Header.Set("Grpc-Timeout", formatTimeout(timeout))
		}
	}

	// ─── 4. Send and receive ──────────────────────────────────────────
	httpResp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer httpResp.Body.Close()

	// gRPC HTTP responses always start with HTTP/2 status 200; the real
	// status is in the trailers (grpc-status). So a non-200 here means
	// something went wrong before gRPC even started.
	if httpResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(httpResp.Body)
		return nil, fmt.Errorf("http status %d: %s", httpResp.StatusCode, body)
	}

	// ─── 5. Decode the response ───────────────────────────────────────
	//
	// Read the 5-byte gRPC header, then the protobuf payload.
	if _, err := io.ReadFull(httpResp.Body, frame[:5]); err != nil {
		return nil, fmt.Errorf("read grpc header: %w", err)
	}
	payloadLen := binary.BigEndian.Uint32(frame[1:5])
	respBytes := make([]byte, payloadLen)
	if _, err := io.ReadFull(httpResp.Body, respBytes); err != nil {
		return nil, fmt.Errorf("read grpc payload: %w", err)
	}

	// Drain the rest of the body. This must complete so HTTP/2 can
	// stream the trailers-only frame and our Trailer header read below
	// returns the gRPC status.
	if _, err := io.Copy(io.Discard, httpResp.Body); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("drain body: %w", err)
	}

	// gRPC convention: status is in trailers as grpc-status.
	if statusStr := httpResp.Trailer.Get("Grpc-Status"); statusStr != "" && statusStr != "0" {
		return nil, fmt.Errorf("grpc-status: %s (%s)", statusStr, httpResp.Trailer.Get("Grpc-Message"))
	}

	resp := &greetv1.GreetResponse{}
	if err := proto.Unmarshal(respBytes, resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	return resp, nil
}

// formatTimeout turns a time.Duration into the gRPC format:
// a positive decimal count followed by a unit letter ("H", "M", "S",
// "m", "u", "n"). "100m" = 100 milliseconds; "1S" = 1 second.
//
// Real clients round up to a multiple of the chosen unit and cap the
// value at 99999999 to stay within the spec's encoding range. For a
// learning client the simple version below is enough.
func formatTimeout(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dm", d.Milliseconds())
	}
	return fmt.Sprintf("%dS", int(d.Seconds()))
}
