// Package client is a minimal HTTP/3 client — the mirror image of
// http0.9/client: just enough protocol to see the exchange on the wire
// without net/http in the way.
//
// The full sequence for a GET:
//
//	quic.DialAddr ──► QUIC connection (UDP, TLS 1.3 handshake)
//	unidir stream  →  0x00 (control) + SETTINGS
//	bidi stream    →  HEADERS { :method GET, :scheme https,
//	                           :authority localhost:4433, :path /hello }
//	               →  Close()   ← FIN: request over (GET has no body)
//	bidi stream    ←  HEADERS { :status 200, content-type text/plain }
//	               ←  DATA "Hello, HTTP/3!\n"
//	               ←  FIN      ← response over
//
// There is no "request line" to write: the method, scheme, authority, and
// path that HTTP/1.1 squeezed onto one text line arrive as QPACK-encoded
// pseudo-headers in the HEADERS frame.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/quic-go/qpack"
	"github.com/quic-go/quic-go"

	"github.com/kianooshaz/http-from-scratch/http3/frames"
)

// streamTypeControl is the unidirectional control stream type (RFC 9114 §6.2).
const streamTypeControl uint64 = 0x0

// Response is a parsed HTTP/3 response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Client talks HTTP/3 over QUIC. Against the example server's self-signed
// certificate, set TLSClientConfig.InsecureSkipVerify.
type Client struct {
	TLSClientConfig *tls.Config
}

// Get fetches rawurl with a single GET on a fresh QUIC connection.
func (c *Client) Get(rawurl string) (*Response, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}

	tlsConf := c.TLSClientConfig.Clone()
	tlsConf.NextProtos = []string{"h3"}

	qconn, err := quic.DialAddr(context.Background(), host, tlsConf, nil)
	if err != nil {
		return nil, fmt.Errorf("quic dial %s: %w", host, err)
	}
	defer qconn.CloseWithError(quic.ApplicationErrorCode(frames.ErrH3NoError), "")

	// Every HTTP/3 endpoint must open a control stream whose first frame
	// is SETTINGS (RFC 9114 §7.2.4) — servers are entitled to treat its
	// absence as H3_MISSING_SETTINGS. Ours disables the QPACK dynamic
	// table, matching what we advertise from the server side.
	cs, err := qconn.OpenUniStream()
	if err != nil {
		return nil, err
	}
	if _, err := cs.Write(frames.AppendVarint(nil, streamTypeControl)); err != nil {
		return nil, err
	}
	if err := frames.Write(cs, frames.FrameSettings, frames.EncodeSettings(
		[2]uint64{frames.SettingsQPACKMaxTableCapacity, 0},
		[2]uint64{frames.SettingsQPACKBlockedStreams, 0},
	)); err != nil {
		return nil, err
	}

	// The request: one HEADERS frame on a fresh bidirectional stream.
	stream, err := qconn.OpenStream()
	if err != nil {
		return nil, err
	}
	var block bytes.Buffer
	enc := qpack.NewEncoder(&block)
	pseudo := [][2]string{
		{":method", "GET"},
		{":scheme", "https"},
		{":authority", u.Host},
		{":path", u.RequestURI()}, // path + query
	}
	for _, kv := range pseudo {
		if err := enc.WriteField(qpack.HeaderField{Name: kv[0], Value: kv[1]}); err != nil {
			return nil, err
		}
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	if err := frames.Write(stream, frames.FrameHeaders, block.Bytes()); err != nil {
		return nil, err
	}
	// FIN: end of request. A GET has no body, so closing the write side
	// right after HEADERS is the whole request.
	if err := stream.Close(); err != nil {
		return nil, err
	}

	return readResponse(stream)
}

// readResponse collects response frames from the request stream until FIN.
func readResponse(stream *quic.Stream) (*Response, error) {
	resp := &Response{Header: make(http.Header)}
	dec := qpack.NewDecoder()
	var body bytes.Buffer
	headersDone := false

	for {
		f, err := frames.Read(stream)
		if errors.Is(err, io.EOF) {
			break // FIN: response complete
		}
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case frames.FrameHeaders:
			if headersDone {
				// A second HEADERS frame is trailers; this minimal
				// client discards them and stops.
				return resp, nil
			}
			next := dec.Decode(f.Payload)
			for {
				hf, err := next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return nil, fmt.Errorf("qpack-decode response headers: %w", err)
				}
				if hf.Name == ":status" {
					resp.Status, err = strconv.Atoi(hf.Value)
					if err != nil {
						return nil, fmt.Errorf("bad :status %q", hf.Value)
					}
					continue
				}
				resp.Header.Add(http.CanonicalHeaderKey(hf.Name), hf.Value)
			}
			headersDone = true
		case frames.FrameData:
			body.Write(f.Payload)
		default:
			// Reserved, unknown, or push-related frames: skip.
		}
	}
	resp.Body = body.Bytes()
	return resp, nil
}
