// Example shows the HTTP/1.0 server with a couple of demo routes.
//
// Run with:
//
//	go run .
//
//	curl -i http://127.0.0.1:9000/headers
//
// The response should look like:
//
//	HTTP/1.0 200 OK
//	Content-Type: application/json
//	Transfer-Encoding: chunked    ← added by net/http, not us; this example sets Content-Type
//	...
//	{ ... r.Header as JSON ... }
//
// HTTP/1.0 servers will (correctly) close the connection after the body
// is sent. curl shows that as "< HTTP/1.0> close" in -v mode.
package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/kianooshaz/http-from-scratch/http1/server"
)

func main() {
	const addr = "127.0.0.1:9000"

	// Routes you'd find in any small HTTP service.
	mux := http.NewServeMux()
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		// Demonstrates that the server correctly parses request headers
		// before dispatching to the handler. We just mirror them back.
		w.Header().Add("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.Header)
	})

	s := server.Server{Addr: addr, Handler: mux}
	log.Printf("listening on http://%s", addr)
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
