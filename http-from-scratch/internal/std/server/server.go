// Package server is a small showcase of stdlib net/http production patterns:
// middleware, graceful shutdown, and a tiny mux.
//
// ─── Why this file exists ───────────────────────────────────────────────────
//
// The http0.9 / http1 / http1.1 packages are learning artefacts: they
// re-implement HTTP on top of raw TCP. That makes them educational, but
// it also means they omit things every production server needs:
//
//   - request logging (so you can debug traffic);
//   - timeouts (so a slow client can't tie up a goroutine forever);
//   - graceful shutdown (so in-flight requests finish on Ctrl-C).
//
// This package wraps net/http with the minimum set of those patterns.
// It's the contrast piece to the protocol packages: "here is what a
// serious HTTP service looks like in production Go".
//
// The functions are deliberately small and free of dependencies so you
// can read them top-to-bottom and lift the patterns into your own code.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"
)

// New returns an http.Handler with a /health and /hello route, wrapped in
// LoggingMiddleware. It's a copy-paste-ready shape for a small service.
func New() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/hello", hello)
	return LoggingMiddleware(mux)
}

// hello reads a "name" query parameter and replies with a JSON greeting.
// It also demonstrates one of the few headers you'll ever set explicitly:
// Content-Type.
func hello(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "world"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"message": "hello " + name})
}

// ─── Middleware ─────────────────────────────────────────────────────────────
//
// Middleware is just an http.Handler that wraps another. The shape:
//
//	func LoggingMiddleware(next http.Handler) http.Handler {
//	    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//	        // do something before
//	        next.ServeHTTP(w, r)
//	        // do something after
//	    })
//	}
//
// Pattern: anything that runs around a handler is a middleware. Common
// examples: auth, tracing, rate limiting, request IDs, panic recovery.

// LoggingMiddleware logs the method, path, and duration of every request.
//
// In a real service you'd use slog or zap with structured fields instead
// of log.Printf, but the shape is identical.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// ─── Graceful shutdown ─────────────────────────────────────────────────────
//
// net/http.Server has a Shutdown method that:
//   1. stops accepting new connections immediately,
//   2. waits for in-flight handlers to return,
//   3. closes idle keep-alive connections,
//   4. cancels everything past the supplied context deadline.
//
// Pair that with signal.NotifyContext and you get a SIGINT-triggered
// graceful shutdown. The Serve helper here wires that up.

// Serve starts an http.Server on addr in a goroutine, then blocks until
// either the server crashes or the process receives SIGINT. On SIGINT it
// shuts the server down with a 5-second deadline.
//
// Run it from main:
//
//	if err := stdserver.Serve(":8080", stdserver.New()); err != nil {
//	    log.Fatal(err)
//	}
func Serve(addr string, h http.Handler) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: h,
		// ReadTimeout covers the entire request body read, including
		// headers. Without it a slow client can stall a goroutine.
		ReadTimeout: 5 * time.Second,
		// WriteTimeout covers the entire response write. Too short and
		// streaming responses get cut off; too long and a stuck client
		// ties up the goroutine. Tune per workload.
		WriteTimeout: 10 * time.Second,
		// IdleTimeout is how long an idle keep-alive connection stays
		// open before the server closes it. 60s matches Apache/nginx
		// defaults.
		IdleTimeout: 60 * time.Second,
		// ReadHeaderTimeout caps the time spent reading request headers
		// specifically. This is the one Slowloris attacks exploit; set
		// it to something low (a few seconds) in production.
		ReadHeaderTimeout: 2 * time.Second,
	}

	// Channel for the server's fatal error (port in use, etc). Buffered
	// so the goroutine never blocks if we exit before reading.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("server started on %s", addr)
		// ListenAndServe returns http.ErrServerClosed after a clean
		// Shutdown; we filter that out because Shutdown is the expected
		// exit path.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	// signal.NotifyContext gives us a context that's cancelled when the
	// process receives SIGINT. We block until either the server fails or
	// that context fires.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Println("shutting down...")
		// Shutdown waits for in-flight handlers. The 5s deadline is a
		// safety net: if a handler hangs past it, Shutdown returns an
		// error and we forcibly close idle connections.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}
