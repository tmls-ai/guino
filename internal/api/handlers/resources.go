package handlers

import (
	"log/slog"
	"net/http"
	"runtime"

	denruntime "github.com/tmls-ai/guino/internal/engine"
)

// ResourceHandler handles resource status endpoints.
type ResourceHandler struct {
	engine *denruntime.Engine
	logger *slog.Logger
}

// NewResourceHandler creates a new ResourceHandler.
func NewResourceHandler(eng *denruntime.Engine, logger *slog.Logger) *ResourceHandler {
	return &ResourceHandler{engine: eng, logger: logger}
}

// Status handles GET /api/v1/resources.
func (h *ResourceHandler) Status(w http.ResponseWriter, _ *http.Request) {
	event := h.engine.CurrentPressure()
	total, running := h.engine.SandboxCount()
	thresholds := h.engine.PressureThresholds()

	canCreate := event.Level < denruntime.PressureCritical

	var nextThreshold float64
	switch event.Level {
	case denruntime.PressureNormal:
		nextThreshold = thresholds.Warning
	case denruntime.PressureWarning:
		nextThreshold = thresholds.High
	case denruntime.PressureHigh:
		nextThreshold = thresholds.Critical
	case denruntime.PressureCritical:
		nextThreshold = thresholds.Emergency
	case denruntime.PressureEmergency:
		nextThreshold = 1.0
	}

	// Guard against uint64 underflow
	var memFree uint64
	if event.MemoryTotal > event.MemoryUsed {
		memFree = event.MemoryTotal - event.MemoryUsed
	}

	resp := map[string]any{
		"host": map[string]any{
			"memory_total": event.MemoryTotal,
			"memory_used":  event.MemoryUsed,
			"memory_free":  memFree,
			"cpu_cores":    runtime.NumCPU(),
		},
		"sandboxes": map[string]any{
			"active": running,
			"total":  total,
		},
		"pressure": map[string]any{
			"level":          event.Level.String(),
			"score":          event.Score,
			"can_create":     canCreate,
			"next_threshold": nextThreshold,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}
