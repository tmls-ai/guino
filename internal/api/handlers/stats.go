package handlers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
)

// StatsHandler handles stats endpoints.
type StatsHandler struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewStatsHandler creates a new StatsHandler.
func NewStatsHandler(eng *engine.Engine, logger *slog.Logger) *StatsHandler {
	return &StatsHandler{engine: eng, logger: logger}
}

// SandboxStats handles GET /api/v1/sandboxes/{id}/stats.
func (h *StatsHandler) SandboxStats(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	stats, err := h.engine.Stats(r.Context(), id)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, stats)
}

// SystemStats handles GET /api/v1/stats.
func (h *StatsHandler) SystemStats(w http.ResponseWriter, _ *http.Request) {
	sandboxes := h.engine.ListSandboxes()

	running := 0
	stopped := 0
	for _, s := range sandboxes {
		switch s.GetStatus() {
		case "running":
			running++
		case "stopped":
			stopped++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_sandboxes":   len(sandboxes),
		"running_sandboxes": running,
		"stopped_sandboxes": stopped,
	})
}
