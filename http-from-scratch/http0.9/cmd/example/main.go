// Example shows the HTTP/0.9 server end-to-end.
//
// It boots the server in a goroutine, makes a real request to it using the
// in-repo client, prints the response, and then blocks. Probe it with
// curl --http0.9 in another terminal to see the same wire format from an
// outside client.
//
// Try:
//
//	go run . &
//
//	curl --http0.9 http://127.0.0.1:9000/this/is/a/test
package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/kianooshaz/http-from-scratch/http0.9/client"
	"github.com/kianooshaz/http-from-scratch/http0.9/server"
)

func main() {
	const addr = "127.0.0.1:9000"

	// Server.Server mirrors net/http.Server. We register a handler that
	// echoes the path so it's obvious what's happening on the wire.
	s := server.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Only r.Method and r.URL.Path are meaningful in HTTP/0.9.
			fmt.Fprintf(w, "Hello World! (got %s %s)\n", r.Method, r.URL.Path)
		}),
	}

	go func() {
		log.Printf("listening on http://%s", addr)
		if err := s.ListenAndServe(); err != nil {
			log.Fatal(err)
		}
	}()

	// Demo a full request/response cycle in-process. The client opens a
	// new TCP connection, writes "GET /this/is/a/test\r\n", reads until
	// the server hangs up, and returns the body bytes.
	body, err := client.Get(addr, "/this/is/a/test")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("client received: %q\n", string(body))

	select {} // block forever — kill with Ctrl-C
}
