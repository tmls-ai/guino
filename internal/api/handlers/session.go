package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
)

// SessionHandler exposes long-lived shells inside a sandbox.
//
// Exec is one-shot, so nothing survives between calls. Sessions are what an
// agent needs to start a server and then check on it, or run a build and read
// its output later. See internal/engine/session.go for why they are tmux.
type SessionHandler struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewSessionHandler creates a new SessionHandler.
func NewSessionHandler(eng *engine.Engine, logger *slog.Logger) *SessionHandler {
	return &SessionHandler{engine: eng, logger: logger}
}

type createSessionRequest struct {
	Name string `json:"name"`
}

type sessionExecRequest struct {
	Command string `json:"command"`
}

type sessionLogsResponse struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

// writeSessionError maps engine errors onto status codes.
func (h *SessionHandler) writeSessionError(w http.ResponseWriter, sandbox string, err error) {
	switch {
	case errors.Is(err, engine.ErrNotFound):
		writeError(w, http.StatusNotFound, "sandbox not found")
	case errors.Is(err, engine.ErrNotRunning):
		writeError(w, http.StatusConflict, "sandbox is not running")
	case errors.Is(err, engine.ErrSessionUnsupported):
		// 501, not 500: the request is valid and the server is healthy — this
		// image simply cannot do sessions.
		writeError(w, http.StatusNotImplemented, "sandbox image does not provide tmux")
	case errors.Is(err, engine.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, engine.ErrSessionExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, engine.ErrInvalidSession):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		h.logger.Error("session operation failed", "sandbox", sandbox, "error", err)
		writeError(w, http.StatusInternalServerError, "session operation failed")
	}
}

// Create handles POST /api/v1/sandboxes/{id}/sessions.
func (h *SessionHandler) Create(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req createSessionRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	if err := h.engine.CreateSession(r.Context(), id, req.Name); err != nil {
		h.writeSessionError(w, id, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name, "status": "created"})
}

// List handles GET /api/v1/sandboxes/{id}/sessions.
func (h *SessionHandler) List(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	names, err := h.engine.ListSessions(r.Context(), id)
	if err != nil {
		h.writeSessionError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, names)
}

// Exec handles POST /api/v1/sandboxes/{id}/sessions/{name}/exec.
//
// Returns 202: the command is now running in the session, and its output is read
// back through Logs. Anything else would be a lie for a command that outlives the
// request, which is the whole reason sessions exist.
func (h *SessionHandler) Exec(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name := chi.URLParam(r, "name")

	var req sessionExecRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := h.engine.SessionExec(r.Context(), id, name, req.Command); err != nil {
		h.writeSessionError(w, id, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"name": name, "status": "running"})
}

// Logs handles GET /api/v1/sandboxes/{id}/sessions/{name}/logs?lines=N.
func (h *SessionHandler) Logs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name := chi.URLParam(r, "name")

	lines := 0
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "lines must be a non-negative integer")
			return
		}
		lines = n
	}

	out, err := h.engine.SessionLogs(r.Context(), id, name, lines)
	if err != nil {
		h.writeSessionError(w, id, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionLogsResponse{Name: name, Output: out})
}

// Delete handles DELETE /api/v1/sandboxes/{id}/sessions/{name}.
func (h *SessionHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	name := chi.URLParam(r, "name")

	if err := h.engine.DeleteSession(r.Context(), id, name); err != nil {
		h.writeSessionError(w, id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
