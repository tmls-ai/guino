package handlers

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
)

func setOf(keys ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

// These tests pin two response-contract invariants the SDKs depend on:
//
//  1. `ports` is present in the JSON IFF the sandbox has at least one port
//     mapping (omitempty on a nil/empty slice). The SDKs treat "key absent"
//     and "[]" identically as "no published ports"; a future struct change
//     that drops omitempty or renames the field would silently break that.
//  2. The wire shape of createSandboxRequest / sandboxResponse is stable —
//     the exact JSON key set is asserted so a rename/retag is a loud,
//     test-breaking change rather than a silent SDK incompatibility.

func jsonKeys(t *testing.T, v any) map[string]struct{} {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))
	keys := make(map[string]struct{}, len(m))
	for k := range m {
		keys[k] = struct{}{}
	}
	return keys
}

func TestSandboxResponse_PortsOmittedWhenEmpty(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())

	req := httptest.NewRequest("POST", "/api/v1/sandboxes",
		bytes.NewBufferString(`{"image":"test:latest"}`))
	w := httptest.NewRecorder()
	h.Create(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// Raw key check: with no ports, omitempty must drop the key entirely so
	// SDKs see "absent", not "null"/"[]".
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	_, present := raw["ports"]
	assert.False(t, present, "ports key must be absent when there are no port mappings")
}

func TestSandboxResponse_PortsPresentAndRoundTrips(t *testing.T) {
	eng := newTestEngine(t)
	h := NewSandboxHandler(eng, slog.Default())

	// Protocol intentionally upper-case to prove the validator normalizes it
	// to canonical lower-case "tcp" on the round-trip.
	body := `{"image":"test:latest","ports":[{"sandbox_port":8080,"host_port":49160,"protocol":"TCP"}]}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	h.Create(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	_, present := raw["ports"]
	require.True(t, present, "ports key must be present when port mappings exist")

	var resp sandboxResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Ports, 1)
	assert.Equal(t, 8080, resp.Ports[0].SandboxPort)
	assert.Equal(t, 49160, resp.Ports[0].HostPort)
	assert.Equal(t, "tcp", resp.Ports[0].Protocol, "protocol must be normalized to canonical lower-case tcp")
}

func TestPortHandler_List_RoundTrip(t *testing.T) {
	eng := newTestEngine(t)
	sh := NewSandboxHandler(eng, slog.Default())
	ph := NewPortHandler(eng, slog.Default())

	r := chi.NewRouter()
	r.Post("/api/v1/sandboxes", sh.Create)
	r.Get("/api/v1/sandboxes/{id}/ports", ph.List)

	body := `{"image":"test:latest","ports":[{"sandbox_port":3000,"host_port":49152,"protocol":"tcp"}]}`
	req := httptest.NewRequest("POST", "/api/v1/sandboxes", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var created sandboxResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created))

	req = httptest.NewRequest("GET", "/api/v1/sandboxes/"+created.ID+"/ports", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var ports []struct {
		SandboxPort int    `json:"sandbox_port"`
		HostPort    int    `json:"host_port"`
		Protocol    string `json:"protocol"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &ports))
	require.Len(t, ports, 1)
	assert.Equal(t, 3000, ports[0].SandboxPort)
	assert.Equal(t, 49152, ports[0].HostPort)
	assert.Equal(t, "tcp", ports[0].Protocol)
}

func TestResponseStructShapeStable(t *testing.T) {
	// Fully-populated sandboxResponse: every field must serialize. If a field
	// is renamed/retagged or an omitempty is added where it must not be, this
	// key set changes and the test fails loudly.
	now := time.Now().UTC()
	full := sandboxResponse{
		ID:        "id-1",
		Image:     "img:1",
		Status:    runtime.StatusRunning,
		CreatedAt: now,
		ExpiresAt: now,
		Ports:     []runtime.PortMapping{{SandboxPort: 1, HostPort: 2, Protocol: "tcp"}},
	}
	assert.Equal(t,
		setOf("id", "image", "status", "created_at", "expires_at", "ports"),
		jsonKeys(t, full),
		"sandboxResponse wire shape changed — SDKs decode these keys")

	// Zero value: `ports` MUST be omitted (nil slice + omitempty) — this is
	// the SDK contract "absent == no published ports". `expires_at` is
	// deliberately NOT asserted absent: encoding/json's omitempty does not
	// apply to a zero time.Time (it's a non-empty struct), so expires_at is
	// always on the wire as the zero instant. That is a known, stable wire
	// fact pinned here so neither half drifts silently.
	assert.Equal(t,
		setOf("id", "image", "status", "created_at", "expires_at"),
		jsonKeys(t, sandboxResponse{}),
		"sandboxResponse zero value: ports omitted, expires_at present (time.Time/omitempty)")

	// createSandboxRequest: network_mode and ports must both be omitempty so
	// an unset request never sends them (server-default + no-ports contract).
	assert.Equal(t,
		setOf("image"),
		jsonKeys(t, createSandboxRequest{Image: "x"}),
		"an image-only create request must not carry network_mode/ports")
	withNet := createSandboxRequest{Image: "x", NetworkMode: "none"}
	keys := jsonKeys(t, withNet)
	_, hasNet := keys["network_mode"]
	assert.True(t, hasNet, "network_mode must serialize when set")
}
