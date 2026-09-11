package handlers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
)

// SnapshotHandler handles snapshot operations.
type SnapshotHandler struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewSnapshotHandler creates a new SnapshotHandler.
func NewSnapshotHandler(eng *engine.Engine, logger *slog.Logger) *SnapshotHandler {
	return &SnapshotHandler{engine: eng, logger: logger}
}

type createSnapshotRequest struct {
	Name string `json:"name"`
}

// Create handles POST /api/v1/sandboxes/{id}/snapshots.
func (h *SnapshotHandler) Create(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req createSnapshotRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	snap, err := h.engine.Snapshot(r.Context(), id, req.Name)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, snap)
}

// List handles GET /api/v1/sandboxes/{id}/snapshots.
func (h *SnapshotHandler) List(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	snapshots, err := h.engine.ListSnapshots(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, snapshots)
}

// Restore handles POST /api/v1/snapshots/{snapshotId}/restore.
func (h *SnapshotHandler) Restore(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")

	sandbox, err := h.engine.RestoreSnapshot(r.Context(), snapshotID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":     sandbox.ID,
		"status": string(sandbox.GetStatus()),
	})
}

// Delete handles DELETE /api/v1/snapshots/{snapshotId}.
func (h *SnapshotHandler) Delete(w http.ResponseWriter, r *http.Request) {
	snapshotID := chi.URLParam(r, "snapshotId")

	if err := h.engine.DeleteSnapshot(r.Context(), snapshotID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
