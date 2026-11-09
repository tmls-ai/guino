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

// The host API a controller codes against: labels round trip, lease and
// archive answers, drain refusing creates with 409, and the stats fields.
func TestHostAPIContract(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())
	r := chi.NewRouter()
	r.Post("/api/v1/sandboxes", h.Create)
	r.Get("/api/v1/sandboxes/{id}", h.Get)
	r.Post("/api/v1/sandboxes/{id}/lease", h.Lease)
	r.Post("/api/v1/sandboxes/{id}/archive", h.Archive)
	r.Post("/api/v1/drain", h.Drain)
	r.Get("/api/v1/stats", NewStatsHandler(eng, slog.Default()).SystemStats)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewBufferString(body)))
		return w
	}

	w := do("POST", "/api/v1/sandboxes", `{"image":"test:latest","labels":{"project":"p1"}}`)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))
	id := created["id"].(string)
	assert.Equal(t, map[string]any{"project": "p1"}, created["labels"])

	assert.Equal(t, http.StatusBadRequest, do("POST", "/api/v1/sandboxes/"+id+"/lease", `{"ttl_seconds":0}`).Code)
	assert.Equal(t, http.StatusNotFound, do("POST", "/api/v1/sandboxes/nope/lease", `{"ttl_seconds":120}`).Code)
	w = do("POST", "/api/v1/sandboxes/"+id+"/lease", `{"ttl_seconds":120}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "leased_until")

	w = do("GET", "/api/v1/sandboxes/"+id, "")
	require.Equal(t, http.StatusOK, w.Code)
	for _, field := range []string{"leased_until", "last_leased_at", "last_activity", "labels"} {
		assert.Contains(t, w.Body.String(), `"`+field+`"`)
	}

	// No state target: nothing to archive to.
	assert.Equal(t, http.StatusConflict, do("POST", "/api/v1/sandboxes/"+id+"/archive", "").Code)
	assert.Equal(t, http.StatusNotFound, do("POST", "/api/v1/sandboxes/nope/archive?force=true", "").Code)
	w = do("POST", "/api/v1/sandboxes", `{"image":"test:latest","state":{"bucket":"b","archive_key":"k","paths":["/root"]}}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "state needs writable_rootfs")

	w = do("POST", "/api/v1/drain", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"draining":true,"count":1}`, w.Body.String())
	w = do("POST", "/api/v1/sandboxes", `{"image":"test:latest"}`)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "draining")

	w = do("GET", "/api/v1/stats", "")
	require.Equal(t, http.StatusOK, w.Code)
	var stats map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &stats))
	assert.Equal(t, true, stats["draining"])
	assert.EqualValues(t, 50, stats["slots"])
	assert.EqualValues(t, 1, stats["count"])
	assert.EqualValues(t, 0, stats["creating"])
	assert.EqualValues(t, 0, stats["archiving"])
	assert.EqualValues(t, 1, stats["total_sandboxes"])
	sbs := stats["sandboxes"].([]any)
	require.Len(t, sbs, 1)
	first := sbs[0].(map[string]any)
	assert.Equal(t, id, first["id"])
	assert.Contains(t, first, "leased_until")
}

// A restore_key that does not exist is the caller's to fix: 422 with a code,
// and no slot is taken.
func TestCreateWithAMissingRestoreKeyAnswers422(t *testing.T) {
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>"))
	}))
	t.Cleanup(s3.Close)
	st, err := store.NewBoltStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	eng := engine.NewEngine(enginetest.NewMockRuntime(), st,
		config.SandboxConfig{DefaultImage: "test:latest", DefaultTimeout: time.Minute, MaxSandboxes: 5,
			State: config.StateConfig{Bucket: "b", KeyPrefix: "p1/"}},
		config.S3Config{Endpoint: s3.URL, Region: "us-east-1", AccessKey: "k", SecretKey: "s", AllowInternalEndpoint: true},
		config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
	h := NewSandboxHandler(eng, slog.Default())

	w := httptest.NewRecorder()
	h.Create(w, httptest.NewRequest("POST", "/api/v1/sandboxes", bytes.NewBufferString(
		`{"image":"test:latest","writable_rootfs":true,"state":{"bucket":"b","restore_key":"p1/gone.tar.zst","archive_key":"p1/next.tar.zst","paths":["/root"]}}`)))
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":"restore_key_not_found"`)
	assert.Equal(t, 0, eng.HostStats().Count)
}
