package main

import "net/http"

// WithVersionHeader sets header to version on every response. Pre-setting it
// (rather than in httputil.ReverseProxy's ModifyResponse) covers every
// response path uniformly, including 403/503 error paths: ReverseProxy only
// adds backend headers via copyHeader, it never clears what's already on the
// ResponseWriter.
//
// version is taken as a parameter rather than read from the package-level
// Version var directly, per this package's own convention against reaching
// for globals inside a component (see CLAUDE.md) — callers pass Version in.
func WithVersionHeader(header, version string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(header, version)
		next.ServeHTTP(w, r)
	})
}
