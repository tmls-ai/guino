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
	// HostBinds requests host directories be mounted into the sandbox. Every
	// entry is validated against the operator's allowed roots before anything is
	// created, and a request may only narrow a root — never widen one, and never
	// name a path outside them.
	HostBinds []runtime.HostBind `json:"host_binds,omitempty"`
	// Labels are stored on the container and reported by stats, so a caller can
	// find its sandboxes on a host it did not create them on.
	Labels map[string]string `json:"labels,omitempty"`
	// State makes the sandbox's state outlive the host (restore and archive).
	State *runtime.StateTarget `json:"state,omitempty"`
}

type sandboxResponse struct {
	ID        string                `json:"id"`
	Image     string                `json:"image"`
	Status    runtime.SandboxStatus `json:"status"`
	CreatedAt time.Time             `json:"created_at"`
	ExpiresAt time.Time             `json:"expires_at,omitempty"`
	Ports     []runtime.PortMapping `json:"ports,omitempty"`
	IP        string                `json:"ip,omitempty"`
	Labels    map[string]string     `json:"labels,omitempty"`
	// Lease and activity times; omitted until first set.
	LeasedUntil  time.Time `json:"leased_until,omitzero"`
	LastLeasedAt time.Time `json:"last_leased_at,omitzero"`
	LastActivity time.Time `json:"last_activity,omitzero"`
}

func toSandboxResponse(s *engine.Sandbox) sandboxResponse {
	a := s.Activity()
	return sandboxResponse{
		ID:           s.ID,
		Image:        s.Image,
		Status:       s.GetStatus(),
		CreatedAt:    s.CreatedAt,
		ExpiresAt:    s.Expiry(),
		Ports:        s.Ports,
		Labels:       s.Config.Labels,
		LeasedUntil:  a.LeasedUntil,
		LastLeasedAt: a.LastLeasedAt,
		LastActivity: a.LastActivity,
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
		HostBinds:      req.HostBinds,
		Labels:         req.Labels,
		State:          req.State,
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
		if writeAdmissionError(w, err) {
			return
		}
		if errors.Is(err, engine.ErrRestoreKeyNotFound) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "code": "restore_key_not_found"})
			return
		}
		if errors.Is(err, engine.ErrInvalidState) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, engine.ErrHostBindRejected) {
			h.logger.Warn("host bind refused", "error", err)
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		// A missing image is the caller's to fix (wrong name, or the image was
		// never loaded), so it must reach them rather than sit in Guino's log as
		// an opaque 500.
		if errors.Is(err, runtime.ErrImageNotFound) {
			h.logger.Warn("sandbox image not present", "error", err)
			writeError(w, http.StatusUnprocessableEntity, err.Error())
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
		if errors.Is(err, engine.ErrArchiving) || errors.Is(err, engine.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
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
		if errors.Is(err, engine.ErrArchiving) || errors.Is(err, engine.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
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
		if errors.Is(err, engine.ErrArchiving) || errors.Is(err, engine.ErrBusy) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if writeAdmissionError(w, err) {
			return
		}
		h.logger.Error("failed to start sandbox", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to start sandbox")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

type leaseRequest struct {
	TTLSeconds int `json:"ttl_seconds"`
}

// maxLeaseTTL bounds one lease: a caller that dies must not pin a sandbox.
const maxLeaseTTL = time.Hour

// Lease handles POST /api/v1/sandboxes/{id}/lease. While leased, the sandbox
// is neither archived nor reaped, whatever its traffic.
func (h *SandboxHandler) Lease(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req leaseRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 || ttl > maxLeaseTTL {
		writeError(w, http.StatusBadRequest, "ttl_seconds must be between 1 and 3600")
		return
	}
	until, err := h.engine.LeaseSandbox(id, ttl)
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, "sandbox not found")
	case errors.Is(err, engine.ErrArchiving):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		h.logger.Error("failed to lease sandbox", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to lease sandbox")
	default:
		writeJSON(w, http.StatusOK, map[string]time.Time{"leased_until": until})
	}
}

// Archive handles POST /api/v1/sandboxes/{id}/archive[?force=true]. It saves
// the state to the sandbox's archive key and removes it; on success the
// sandbox is gone (404 from then on). Without force, a lease or a held
// operation answers 409.
func (h *SandboxHandler) Archive(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	force := r.URL.Query().Get("force") == "true"
	err := h.engine.ArchiveSandbox(r.Context(), id, force)
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, "sandbox not found")
	case errors.Is(err, engine.ErrArchiving), errors.Is(err, engine.ErrBusy), errors.Is(err, engine.ErrInUse), errors.Is(err, engine.ErrNoState):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		h.logger.Error("failed to archive sandbox", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to archive sandbox")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "archived"})
	}
}

// Drain handles POST /api/v1/drain. From now on every create, start and
// snapshot restore answers 409; count is how many containers still exist, and
// a host may be removed only when a drain answers count 0. Idempotent.
func (h *SandboxHandler) Drain(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"draining": true, "count": h.engine.Drain()})
}
