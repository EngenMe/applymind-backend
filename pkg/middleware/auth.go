// Package middleware holds Chi middleware shared across the API Lambda.
// MVP auth is a single static API key (no users table, no JWT) — see the
// architecture diagram's "API key middleware" note on Lambda 1. This is
// explicitly a placeholder: the diagram's Phase 2 annotation calls for
// replacing this with JWT middleware (golang-jwt/jwt) once a users table
// exists, so callers should not build assumptions about the key's shape or
// permanence into other packages.
package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

const bearerPrefix = "Bearer "

// APIKeyAuth returns Chi-compatible middleware that requires an exact
// "Authorization: Bearer <key>" header matching expectedKey. Comparison uses
// subtle.ConstantTimeCompare so response latency cannot be used to guess the
// key one byte at a time.
//
// expectedKey must be non-empty; an empty key would make every request with
// a missing header pass (empty == empty), which is never correct.
func APIKeyAuth(expectedKey string) func(http.Handler) http.Handler {
	if expectedKey == "" {
		panic("middleware: APIKeyAuth requires a non-empty expectedKey")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")

			if !strings.HasPrefix(header, bearerPrefix) {
				unauthorized(w)
				return
			}

			presented := strings.TrimPrefix(header, bearerPrefix)

			// ConstantTimeCompare requires equal-length inputs to give its
			// timing guarantee; unequal lengths already fail comparison
			// safely, but we still route both cases through it rather than
			// branching on length first, to avoid a length-based timing
			// side channel of our own making.
			match := subtle.ConstantTimeCompare([]byte(presented), []byte(expectedKey)) == 1
			if !match {
				unauthorized(w)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": "unauthorized",
	})
}
