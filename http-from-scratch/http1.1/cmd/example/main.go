// Example shows the HTTP/1.1 server with several demo routes that exercise
// the protocol's signature features: persistent connections, chunked
// request bodies, chunked responses, and routing parameters.
//
// Run with:
//
//	go run .
//
// Then try a few requests from another terminal:
//
//	# Vanilla GET — server emits a chunked response by default.
//	curl -i http://127.0.0.1:9000/headers
//
//	# A POST with a Content-Length — server reads the exact body bytes.
//	curl -i -X POST --data 'hello world' http://127.0.0.1:9000/echo
//
//	# A POST with Transfer-Encoding: chunked — server decodes the chunks.
//	curl -i -X POST -H 'Transfer-Encoding: chunked' \
//	    --data-binary @- http://127.0.0.1:9000/echo/chunked <<'EOF'
//	5
//	hello
//	0
//
//	# Routing with a parameter — server uses Go 1.22+ path values.
//	curl -i http://127.0.0.1:9000/status/418
//
// To confirm persistent connections:
//
//	curl -v --http1.1 http://127.0.0.1:9000/headers
//	# Look for "* Connection #0 to host 127.0.0.1 left intact" on subsequent requests.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/kianooshaz/http-from-scratch/http1.1/server"
)

func main() {
	const addr = "127.0.0.1:9000"

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(".")))

	// /echo — verifies Content-Length request body handling.
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		b, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write(b)
	})

	// /echo/chunked — verifies chunked request body handling: io.Copy
	// streams the chunked decoder straight back to the client, which
	// will produce another chunked response.
	mux.HandleFunc("/echo/chunked", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		_, _ = io.Copy(w, r.Body)
	})

	// /status/{status} — demonstrates Go 1.22+ path-parameter routing.
	mux.HandleFunc("/status/{status}", func(w http.ResponseWriter, r *http.Request) {
		status, err := strconv.ParseInt(r.PathValue("status"), 10, 64)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, fmt.Sprintf("error: %s", err))
			return
		}
		w.WriteHeader(int(status))
	})

	// /headers — echoes request headers back as JSON.
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.Header)
	})

	// /nothing — handler returns without writing anything; the server
	// will emit "0\r\n\r\n" via Flush() and keep the connection alive.
	mux.HandleFunc("/nothing", func(w http.ResponseWriter, _ *http.Request) {})

	s := server.Server{Addr: addr, Handler: mux}
	log.Printf("listening on http://%s", addr)
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
