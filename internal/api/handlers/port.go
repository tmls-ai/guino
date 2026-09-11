package handlers

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
)

// PortHandler handles port forwarding operations.
type PortHandler struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewPortHandler creates a new PortHandler.
func NewPortHandler(eng *engine.Engine, logger *slog.Logger) *PortHandler {
	return &PortHandler{engine: eng, logger: logger}
}

// List handles GET /api/v1/sandboxes/{id}/ports.
func (h *PortHandler) List(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sandbox, err := h.engine.GetSandbox(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "sandbox not found")
		return
	}
	writeJSON(w, http.StatusOK, sandbox.Ports)
}

// portsUnsupportedMsg is the committed 501 body for both dynamic port
// mutation endpoints. Port mappings are fixed at sandbox creation and
// published by Docker itself (HostConfig.PortBindings) only in
// network_mode=bridge; there is no userspace proxy to add/remove at runtime.
// The dead in-process PortForwarder was removed in v9.
const portsUnsupportedMsg = "dynamic port forwarding is not supported: port mappings are fixed at sandbox creation and only published in network_mode=bridge (Docker-native, no runtime proxy)"

// Add handles POST /api/v1/sandboxes/{id}/ports.
func (h *PortHandler) Add(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, portsUnsupportedMsg)
}

// Remove handles DELETE /api/v1/sandboxes/{id}/ports/{port}.
func (h *PortHandler) Remove(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, portsUnsupportedMsg)
}
