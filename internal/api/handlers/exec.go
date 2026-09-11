package handlers

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
)

// ExecHandler handles command execution.
type ExecHandler struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewExecHandler creates a new ExecHandler.
func NewExecHandler(eng *engine.Engine, logger *slog.Logger) *ExecHandler {
	return &ExecHandler{engine: eng, logger: logger}
}

type execRequest struct {
	Cmd     []string          `json:"cmd"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Timeout int               `json:"timeout,omitempty"` // seconds
}

type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Exec handles POST /api/v1/sandboxes/{id}/exec.
func (h *ExecHandler) Exec(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req execRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Cmd) == 0 {
		writeError(w, http.StatusBadRequest, "cmd is required")
		return
	}

	const maxExecTimeout = 5 * time.Minute

	opts := runtime.ExecOpts{
		Cmd:     req.Cmd,
		Env:     req.Env,
		WorkDir: req.WorkDir,
	}
	if req.Timeout > 0 {
		opts.Timeout = time.Duration(req.Timeout) * time.Second
		if opts.Timeout > maxExecTimeout {
			opts.Timeout = maxExecTimeout
		}
	}

	result, err := h.engine.Exec(r.Context(), id, opts)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		if errors.Is(err, engine.ErrNotRunning) {
			writeError(w, http.StatusConflict, "sandbox is not running")
			return
		}
		h.logger.Error("exec failed", "sandbox", id, "error", err)
		writeError(w, http.StatusInternalServerError, "command execution failed")
		return
	}

	writeJSON(w, http.StatusOK, execResponse{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	})
}
