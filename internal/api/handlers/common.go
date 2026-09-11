package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/tmls-ai/guino/internal/runtime"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

// ServerFeatures advertises optional server capabilities to clients. It is a
// capability hint only — NOT an authentication or authorization signal. SDKs
// use it lazily to fail fast against servers that predate a feature. Keep the
// tokens stable; they are part of the public API surface.
var ServerFeatures = []string{"network_mode", "start", "file_stat", "sandbox_ip", "writable_rootfs"}

// SetVersion sets the build version info.
func SetVersion(v, c, b string) {
	version = v
	commit = c
	buildDate = b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

const maxJSONBodySize = 1 * 1024 * 1024 // 1MB

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, maxJSONBodySize)
	return json.NewDecoder(r.Body).Decode(v)
}

// HealthHandler creates a health check handler that verifies Docker connectivity.
func HealthHandler(rt runtime.Runtime) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		if err := rt.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"status": "unhealthy",
				"reason": "runtime unavailable",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "ok",
			"features": ServerFeatures,
		})
	}
}

// Version returns server version info.
func Version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    version,
		"commit":     commit,
		"build_date": buildDate,
		"features":   ServerFeatures,
	})
}
