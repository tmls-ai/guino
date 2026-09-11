package handlers

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/pathutil"
)

// FileHandler handles file operations.
type FileHandler struct {
	engine *engine.Engine
	logger *slog.Logger
}

// NewFileHandler creates a new FileHandler.
func NewFileHandler(eng *engine.Engine, logger *slog.Logger) *FileHandler {
	return &FileHandler{engine: eng, logger: logger}
}

func validatePath(path string) error {
	return pathutil.ValidatePath(path)
}

// safeFilename strips characters that could cause header injection.
var unsafeFilenameRe = regexp.MustCompile(`[^\w\.\-]`)

func safeFilename(name string) string {
	safe := unsafeFilenameRe.ReplaceAllString(name, "_")
	if safe == "" {
		safe = "download"
	}
	return safe
}

// ReadFile handles GET /api/v1/sandboxes/{id}/files?path=/foo.
func (h *FileHandler) ReadFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	content, err := h.engine.ReadFile(r.Context(), id, path)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("failed to read file", "sandbox", id, "path", path, "error", err)
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- raw sandbox file bytes served as an octet-stream body,
	// never interpreted as HTML by the browser; this is a file-read API, not
	// a rendered page. Taint here is by design (the caller asked for the file).
	_, _ = w.Write(content)
}

// WriteFile handles PUT /api/v1/sandboxes/{id}/files?path=/foo.
func (h *FileHandler) WriteFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	content, err := io.ReadAll(io.LimitReader(r.Body, 100*1024*1024)) // 100MB limit
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	if err := h.engine.WriteFile(r.Context(), id, path, content); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// ListDir handles GET /api/v1/sandboxes/{id}/files/list?path=/.
func (h *FileHandler) ListDir(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	files, err := h.engine.ListDir(r.Context(), id, path)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, files)
}

// Stat handles GET /api/v1/sandboxes/{id}/files/stat?path=/foo.
func (h *FileHandler) Stat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	info, err := h.engine.Stat(r.Context(), id, path)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		// For a known, running sandbox a stat failure means the path does not
		// exist; surface it as a 404 rather than a server error.
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	writeJSON(w, http.StatusOK, info)
}

// MkDir handles POST /api/v1/sandboxes/{id}/files/mkdir?path=/foo.
func (h *FileHandler) MkDir(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.engine.MkDir(r.Context(), id, path); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// RemoveFile handles DELETE /api/v1/sandboxes/{id}/files?path=/foo.
func (h *FileHandler) RemoveFile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.engine.RemoveFile(r.Context(), id, path); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// Upload handles POST /api/v1/sandboxes/{id}/files/upload (multipart).
func (h *FileHandler) Upload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// #nosec G120 -- bounded to 100MB by the 100<<20 limit argument; G120's
	// "unbounded" heuristic does not recognize the in-call cap.
	if err := r.ParseMultipartForm(100 << 20); err != nil { // 100MB
		writeError(w, http.StatusBadRequest, "failed to parse multipart form")
		return
	}

	path := r.FormValue("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path field is required")
		return
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer func() { _ = file.Close() }()

	const maxUploadSize = 100 * 1024 * 1024 // 100MB
	content, err := io.ReadAll(io.LimitReader(file, maxUploadSize+1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read file")
		return
	}
	if len(content) > maxUploadSize {
		writeError(w, http.StatusRequestEntityTooLarge, "file exceeds 100MB limit")
		return
	}

	if err := h.engine.WriteFile(r.Context(), id, path, content); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// Download handles GET /api/v1/sandboxes/{id}/files/download?path=/foo.
func (h *FileHandler) Download(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	if err := validatePath(path); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	content, err := h.engine.ReadFile(r.Context(), id, path)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		h.logger.Error("internal error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	filename := safeFilename(filepath.Base(path))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.WriteHeader(http.StatusOK)
	// #nosec G705 -- octet-stream attachment download of raw sandbox bytes;
	// Content-Disposition: attachment forces a download, never HTML render.
	_, _ = w.Write(content)
}
