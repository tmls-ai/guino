package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/engine/enginetest"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

// Lease, archive and drain control the host, so a server without auth refuses
// them, as it refuses host binds.
func TestHostControlRoutesNeedAuth(t *testing.T) {
	st, err := store.NewBoltStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	mock := enginetest.NewMockRuntime()
	eng := engine.NewEngine(mock, st, config.SandboxConfig{DefaultImage: "x", DefaultTimeout: time.Minute, MaxSandboxes: 5},
		config.S3Config{}, config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
	t.Cleanup(func() { eng.Shutdown(context.Background()) })

	for _, auth := range []bool{false, true} {
		cfg := config.DefaultConfig()
		cfg.Auth.Enabled = auth
		cfg.Auth.APIKeys = []string{"k"}
		r := chi.NewRouter()
		RegisterRoutes(r, eng, mock, cfg, slog.Default())
		for _, path := range []string{"/api/v1/drain", "/api/v1/sandboxes/x/lease", "/api/v1/sandboxes/x/archive"} {
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Header.Set("X-API-Key", "k")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if !auth && w.Code != http.StatusForbidden {
				t.Errorf("auth off, %s: %d, want 403", path, w.Code)
			}
			if auth && w.Code == http.StatusForbidden {
				t.Errorf("auth on, %s: 403", path)
			}
		}
	}
}
