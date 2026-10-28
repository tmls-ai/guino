package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// Auth returns a middleware that validates API keys using constant-time comparison.
func Auth(validKeys []string) func(http.Handler) http.Handler {
	// Pre-hash keys for constant-time comparison
	keyHashes := make([][]byte, len(validKeys))
	for i, k := range validKeys {
		h := sha256.Sum256([]byte(k))
		keyHashes[i] = h[:]
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("X-API-Key")

			if key == "" {
				markAuthFailed(r)
				http.Error(w, `{"error":"missing API key"}`, http.StatusUnauthorized)
				return
			}

			// Constant-time comparison to prevent timing attacks
			keyHash := sha256.Sum256([]byte(key))
			valid := false
			for _, h := range keyHashes {
				if subtle.ConstantTimeCompare(keyHash[:], h) == 1 {
					valid = true
					// Don't break early - keep iterating for constant time
				}
			}

			if !valid {
				markAuthFailed(r)
				http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
