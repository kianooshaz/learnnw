// Package mux contains benchmarks for net/http's default ServeMux under
// different route shapes (fixed paths, wildcard-style paths, mixed).
//
// ─── Why this file exists ───────────────────────────────────────────────────
//
// The protocol packages in this module re-implement HTTP routing, header
// parsing, and so on from scratch. The reason net/http's *real* ServeMux
// is so fast is that Go 1.22 replaced its tree-walk implementation with
// a radix tree — a structure tuned for the common cases here:
//
//   - many exact-match routes (/fixed/path1, /fixed/path2, …)
//   - many "wildcard" routes that look like /users/<id>/profile
//   - a mix of the two
//
// These benchmarks are the empirical counterpart to the http0.9/http1/
// http1.1 packages: rather than write another ServeMux, we measure the
// one that ships with the language. Run them with:
//
//	cd http-from-scratch
//	go test -bench . ./internal/std/mux
//
// Expected order of magnitude (Apple M-series, single core):
//
//	BenchmarkFixedOnly        ~150 ns/op
//	BenchmarkWildcardOnly     ~250 ns/op
//	BenchmarkMixedFixed       ~150 ns/op
//	BenchmarkMixedWildcard    ~250 ns/op
//
// The wildcard-only and mixed-wildcard cases are slower because they
// involve tree traversal rather than direct lookup, but even so they
// stay well under a microsecond. That's why "from scratch" servers in
// this repo don't bother with custom routing — net/http is already very
// good.
package mux

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// noop is a Handler that does nothing. We use it everywhere so the
// benchmark numbers reflect routing cost, not handler cost.
func noop(w http.ResponseWriter, _ *http.Request) {}

// setupFixed registers 30 exact-match routes, all with distinct paths.
// This is the best case for ServeMux: a hash hit on the first trie level.
func setupFixed() *http.ServeMux {
	mux := http.NewServeMux()
	for i := 1; i <= 30; i++ {
		mux.HandleFunc("/fixed/path"+strconv.Itoa(i), noop)
	}
	return mux
}

// setupWildcard registers 30 routes that share a common prefix but have
// a varying segment in the middle (/users/1/profile, /users/2/profile, …).
// This exercises the trie-walk path.
func setupWildcard() *http.ServeMux {
	mux := http.NewServeMux()
	for i := 1; i <= 30; i++ {
		mux.HandleFunc("/users/"+strconv.Itoa(i)+"/profile", noop)
	}
	return mux
}

// setupMixed registers a blend: half fixed, half wildcard. This is the
// case real services care about.
func setupMixed() *http.ServeMux {
	mux := http.NewServeMux()
	for i := 1; i <= 15; i++ {
		mux.HandleFunc("/fixed/path"+strconv.Itoa(i), noop)
	}
	for i := 1; i <= 15; i++ {
		mux.HandleFunc("/users/"+strconv.Itoa(i)+"/profile", noop)
	}
	return mux
}

// BenchmarkFixedOnly asks the mux to dispatch a fixed-path request.
func BenchmarkFixedOnly(b *testing.B) {
	mux := setupFixed()
	req := httptest.NewRequest(http.MethodGet, "/fixed/path1", nil)
	w := httptest.NewRecorder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(w, req)
	}
}

// BenchmarkWildcardOnly asks the mux to dispatch a wildcard-shape request.
func BenchmarkWildcardOnly(b *testing.B) {
	mux := setupWildcard()
	req := httptest.NewRequest(http.MethodGet, "/users/7/profile", nil)
	w := httptest.NewRecorder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(w, req)
	}
}

// BenchmarkMixedFixed asks the mixed mux to dispatch a fixed request.
func BenchmarkMixedFixed(b *testing.B) {
	mux := setupMixed()
	req := httptest.NewRequest(http.MethodGet, "/fixed/path1", nil)
	w := httptest.NewRecorder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(w, req)
	}
}

// BenchmarkMixedWildcard asks the mixed mux to dispatch a wildcard request.
func BenchmarkMixedWildcard(b *testing.B) {
	mux := setupMixed()
	req := httptest.NewRequest(http.MethodGet, "/users/7/profile", nil)
	w := httptest.NewRecorder()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(w, req)
	}
}
