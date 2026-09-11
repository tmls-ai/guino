package handlers

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/engine/enginetest"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

func newTestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewBoltStore(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	mock := enginetest.NewMockRuntime()
	cfg := config.SandboxConfig{
		DefaultImage:    "test:latest",
		DefaultTimeout:  10 * time.Minute,
		MaxSandboxes:    50,
		DefaultCPU:      1_000_000_000,
		DefaultMemory:   512 * 1024 * 1024,
		DefaultPidLimit: 256,
	}

	eng := engine.NewEngine(mock, st, cfg, config.S3Config{}, config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
	return eng
}

func TestSandboxHandler_Create(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())

	body := `{"image": "ubuntu:24.04"}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Create(w, req)

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp sandboxResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.NotEmpty(t, resp.ID)
	assert.Equal(t, "running", string(resp.Status))
}

func TestSandboxHandler_List(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())

	// Create a sandbox first
	body := `{"image": "test:latest"}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.Create(w, req)

	// List
	req = httptest.NewRequest("GET", "/api/v1/sandboxes", nil)
	w = httptest.NewRecorder()
	h.List(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp []sandboxResponse
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Len(t, resp, 1)
}

func TestSandboxHandler_Get_NotFound(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())

	r := chi.NewRouter()
	r.Get("/api/v1/sandboxes/{id}", h.Get)

	req := httptest.NewRequest("GET", "/api/v1/sandboxes/nonexistent", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSandboxHandler_Delete(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())

	r := chi.NewRouter()
	r.Post("/api/v1/sandboxes", h.Create)
	r.Delete("/api/v1/sandboxes/{id}", h.Delete)

	// Create
	body := `{"image": "test:latest"}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var created sandboxResponse
	json.Unmarshal(w.Body.Bytes(), &created)

	// Delete
	req = httptest.NewRequest("DELETE", "/api/v1/sandboxes/"+created.ID, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestSandboxHandler_InvalidBody(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())

	req := httptest.NewRequest("POST", "/api/v1/sandboxes", bytes.NewBufferString("invalid json"))
	w := httptest.NewRecorder()
	h.Create(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
