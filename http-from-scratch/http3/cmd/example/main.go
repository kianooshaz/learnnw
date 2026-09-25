// Command example serves HTTP/3 on a UDP port, or fetches a URL when run
// with -get.
//
// QUIC *is* TLS: the certificate is part of the transport handshake, so
// unlike the TCP servers in this repo there is no plaintext mode. The
// demo generates a self-signed certificate in memory.
//
//	# server
//	go run ./http3/cmd/example
//
//	# fetch with this repo's own client (tolerates the self-signed cert)
//	go run ./http3/cmd/example -get https://localhost:4433/hello
//
//	# or with an h3-capable curl
//	curl --http3-only -k https://localhost:4433/hello
//
// Routes:
//
//	/hello   a small body — one HEADERS frame, one DATA frame, FIN
//	/stream  five writes spread over a second — each Write becomes a DATA
//	         frame, streamed with no Content-Length and no chunked
//	         encoding: the QUIC stream frames everything
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/kianooshaz/http-from-scratch/http3/client"
	"github.com/kianooshaz/http-from-scratch/http3/server"
)

func main() {
	get := flag.String("get", "", "fetch this URL with HTTP/3 and exit (client mode)")
	addr := flag.String("addr", ":4433", "UDP listen address (server mode)")
	flag.Parse()

	if *get != "" {
		if err := runClient(*get); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runServer(*addr); err != nil {
		log.Fatal(err)
	}
}

func runServer(addr string) error {
	cert, err := selfSigned()
	if err != nil {
		return fmt.Errorf("generate self-signed cert: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, HTTP/3!")
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		for i := range 5 {
			fmt.Fprintf(w, "chunk %d\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	srv := &server.Server{
		Addr:    addr,
		Handler: mux,
		TLSConf: &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"h3"},
		},
	}
	log.Printf("http/3: serving UDP %s — try: go run . -get https://localhost%s/hello", addr, addr)
	return srv.ListenAndServe()
}

func runClient(rawurl string) error {
	cl := &client.Client{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // demo cert
	}
	resp, err := cl.Get(rawurl)
	if err != nil {
		return err
	}
	fmt.Printf("HTTP/3 %d %s\n", resp.Status, http.StatusText(resp.Status))
	for k, vs := range resp.Header {
		for _, v := range vs {
			fmt.Printf("%s: %s\n", k, v)
		}
	}
	fmt.Printf("\n%s", resp.Body)
	return nil
}

// selfSigned generates a throwaway certificate for localhost. Production
// servers load a real one; the shape of the TLS config is the same.
func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
