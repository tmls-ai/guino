package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

// SandboxHandler handles sandbox CRUD operations.
type SandboxHandler struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewSandboxHandler creates a new SandboxHandler.
func NewSandboxHandler(eng *engine.Engine, logger *slog.Logger) *SandboxHandler {
	return &SandboxHandler{engine: eng, logger: logger}
}

type createSandboxRequest struct {
	Image   string                 `json:"image"`
	Env     map[string]string      `json:"env,omitempty"`
	WorkDir string                 `json:"workdir,omitempty"`
	Timeout int                    `json:"timeout,omitempty"` // seconds
	CPU     int64                  `json:"cpu,omitempty"`
	Memory  int64                  `json:"memory,omitempty"`
	Ports   []runtime.PortMapping  `json:"ports,omitempty"`
	Storage *runtime.StorageConfig `json:"storage,omitempty"`
	// WritableRootfs opts out of the read-only rootfs default (secure default
	// is read-only). Needed for heavyweight images (e.g. Chrome+VNC desktop).
	WritableRootfs bool `json:"writable_rootfs,omitempty"`
	// NetworkMode is the per-sandbox override. Only "" (inherit the global
	// default) or "none" (more isolation) are accepted; any other value —
	// including one equal to the global default — is a 400. A per-sandbox
	// override may only INCREASE isolation.
	NetworkMode runtime.NetworkMode `json:"network_mode,omitempty"`
}

type sandboxResponse struct {
	ID        string                `json:"id"`
	Image     string                `json:"image"`
	Status    runtime.SandboxStatus `json:"status"`
	CreatedAt time.Time             `json:"created_at"`
	ExpiresAt time.Time             `json:"expires_at,omitempty"`
	Ports     []runtime.PortMapping `json:"ports,omitempty"`
	IP        string                `json:"ip,omitempty"`
}

func toSandboxResponse(s *engine.Sandbox) sandboxResponse {
	return sandboxResponse{
		ID:        s.ID,
		Image:     s.Image,
		Status:    s.GetStatus(),
		CreatedAt: s.CreatedAt,
		ExpiresAt: s.ExpiresAt,
		Ports:     s.Ports,
	}
}

// Create handles POST /api/v1/sandboxes.
func (h *SandboxHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createSandboxRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cfg := runtime.SandboxConfig{
		Image:          req.Image,
		Env:            req.Env,
		WorkDir:        req.WorkDir,
		CPU:            req.CPU,
		Memory:         req.Memory,
		Ports:          req.Ports,
		Storage:        req.Storage,
		NetworkMode:    req.NetworkMode,
		WritableRootfs: req.WritableRootfs,
	}
	if req.Timeout > 0 {
		cfg.Timeout = time.Duration(req.Timeout) * time.Second
	}

	sandbox, err := h.engine.CreateSandbox(r.Context(), cfg)
	if err != nil {
		var verr *netpolicy.ValidationError
		if errors.As(err, &verr) {
			writeError(w, http.StatusBadRequest, verr.Error())
			return
		}
		if errors.Is(err, engine.ErrLimitReached) {
			writeError(w, http.StatusTooManyRequests, "maximum sandbox limit reached")
			return
		}
		if errors.Is(err, engine.ErrPressureTooHigh) {
			writeError(w, http.StatusServiceUnavailable, "host under critical memory pressure")
			return
		}
		h.logger.Error("failed to create sandbox", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create sandbox")
		return
	}

	writeJSON(w, http.StatusCreated, toSandboxResponse(sandbox))
}

// List handles GET /api/v1/sandboxes.
func (h *SandboxHandler) List(w http.ResponseWriter, _ *http.Request) {
	sandboxes := h.engine.ListSandboxes()
	resp := make([]sandboxResponse, 0, len(sandboxes))
	for _, s := range sandboxes {
		resp = append(resp, toSandboxResponse(s))
	}
	writeJSON(w, http.StatusOK, resp)
}

// Get handles GET /api/v1/sandboxes/{id}.
func (h *SandboxHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sandbox, err := h.engine.GetSandbox(id)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("failed to get sandbox", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to get sandbox")
		return
	}
	resp := toSandboxResponse(sandbox)
	// Enrich with the live container IP (best-effort; absent on stopped
	// sandboxes or non-routable hosts).
	if info, ierr := h.engine.Info(r.Context(), id); ierr == nil {
		resp.IP = info.IP
	}
	writeJSON(w, http.StatusOK, resp)
}

// Delete handles DELETE /api/v1/sandboxes/{id}.
func (h *SandboxHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.engine.DestroySandbox(r.Context(), id); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("failed to destroy sandbox", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to destroy sandbox")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Stop handles POST /api/v1/sandboxes/{id}/stop.
func (h *SandboxHandler) Stop(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.engine.StopSandbox(r.Context(), id); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("failed to stop sandbox", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to stop sandbox")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

// Start handles POST /api/v1/sandboxes/{id}/start.
func (h *SandboxHandler) Start(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.engine.StartSandbox(r.Context(), id); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("failed to start sandbox", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start sandbox")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}
