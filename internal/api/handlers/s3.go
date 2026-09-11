package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/pathutil"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/security/ssrf"
	"github.com/tmls-ai/guino/internal/storage"
)

const (
	// maxS3ImportSize limits the size of a single S3 import (100MB).
	maxS3ImportSize = 100 * 1024 * 1024
	// maxS3ExportSize limits the size of a single S3 export (100MB).
	maxS3ExportSize = 100 * 1024 * 1024
)

// S3Handler handles S3 import/export operations for sandboxes.
type S3Handler struct {
	engine   *engine.Engine
	s3Config config.S3Config
	logger   *slog.Logger
}

// NewS3Handler creates a new S3Handler.
func NewS3Handler(eng *engine.Engine, s3Cfg config.S3Config, logger *slog.Logger) *S3Handler {
	return &S3Handler{engine: eng, s3Config: s3Cfg, logger: logger}
}

// validateEndpoint checks that a user-supplied S3 endpoint is a valid HTTP(S)
// URL and blocks SSRF attempts targeting internal/private networks. It is the
// early defense-in-depth reject for the request-supplied endpoint; the actual
// connection is independently pinned by the storage transport. Both consult
// the SAME internal/security/ssrf predicate, so the early reject and the dial
// agree on exactly one exempt host: the operator-configured endpoint, and only
// when storage.s3.allow_internal_endpoint is enabled.
func (h *S3Handler) validateEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint must use http or https scheme")
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint must have a host")
	}

	canonical, ip, err := ssrf.EndpointHost(endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint host: %w", err)
	}

	allow := h.s3Config.AllowInternalEndpoint
	pinned := h.pinnedServerEndpointSet()

	check := func(addr net.IP) error {
		if !ssrf.DialPermitted(addr, allow, pinned) {
			return fmt.Errorf("endpoint resolves to a blocked address: %s", addr)
		}
		return nil
	}

	if ip != nil {
		return check(ip)
	}
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", canonical)
	if err != nil {
		return fmt.Errorf("cannot resolve endpoint host: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("endpoint host %q resolved to no addresses", canonical)
	}
	for _, addr := range ips {
		if err := check(addr); err != nil {
			return err
		}
	}
	return nil
}

// pinnedServerEndpointSet returns the operator-configured endpoint's resolved
// IP set when the internal-endpoint exemption is active, else nil. It mirrors
// the storage dialer's construction-time pin so the handler's early SSRF
// reject and the actual connection agree on exactly one exempt host. A
// resolution failure yields nil (no exemption) — fail closed.
func (h *S3Handler) pinnedServerEndpointSet() []net.IP {
	if !h.s3Config.AllowInternalEndpoint || h.s3Config.Endpoint == "" {
		return nil
	}
	set, err := ssrf.ResolvePinnedSet(
		func(host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(context.Background(), "ip", host)
		},
		h.s3Config.Endpoint,
	)
	if err != nil {
		return nil
	}
	return set
}

type s3ImportRequest struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	DestPath  string `json:"dest_path"`
	Endpoint  string `json:"endpoint,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	Region    string `json:"region,omitempty"`
}

type s3ImportResponse struct {
	Success         bool   `json:"success"`
	BytesDownloaded int64  `json:"bytes_downloaded"`
	Path            string `json:"path"`
}

// Import handles POST /api/v1/sandboxes/{id}/files/s3-import.
func (h *S3Handler) Import(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req s3ImportRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Bucket == "" || req.Key == "" || req.DestPath == "" {
		writeError(w, http.StatusBadRequest, "bucket, key, and dest_path are required")
		return
	}

	if err := h.validateEndpoint(req.Endpoint); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate dest_path to prevent path traversal
	if err := pathutil.ValidatePath(req.DestPath); err != nil {
		writeError(w, http.StatusBadRequest, "invalid dest_path: "+err.Error())
		return
	}

	// Build S3 config from request + server defaults
	s3Cfg := &runtime.S3SyncConfig{
		Endpoint:  req.Endpoint,
		Bucket:    req.Bucket,
		Region:    req.Region,
		AccessKey: req.AccessKey,
		SecretKey: req.SecretKey,
	}

	creds, err := storage.ResolveS3Credentials(s3Cfg, h.s3Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	client, err := storage.NewS3Client(r.Context(), creds, h.logger)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create S3 client")
		return
	}

	body, size, err := client.Download(r.Context(), req.Bucket, req.Key)
	if err != nil {
		h.logger.Error("s3 download failed", "error", err, "bucket", req.Bucket, "key", req.Key)
		writeError(w, http.StatusInternalServerError, "failed to download from S3")
		return
	}
	defer func() { _ = body.Close() }()

	// Reject early if S3 reported size exceeds limit
	if size > maxS3ImportSize {
		writeError(w, http.StatusBadRequest, "S3 object exceeds maximum import size of 100MB")
		return
	}

	// Read downloaded content with size limit
	limited := io.LimitReader(body, maxS3ImportSize+1)
	var buf bytes.Buffer
	if size > 0 {
		buf.Grow(int(size)) // pre-allocate when size is known
	}
	n, err := buf.ReadFrom(limited)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read S3 object")
		return
	}
	if n > maxS3ImportSize {
		writeError(w, http.StatusBadRequest, "S3 object exceeds maximum import size of 100MB")
		return
	}
	if size == 0 {
		size = n
	}

	// Write to sandbox
	if err := h.engine.WriteFile(r.Context(), id, req.DestPath, buf.Bytes()); err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to write file to sandbox")
		return
	}

	writeJSON(w, http.StatusOK, s3ImportResponse{
		Success:         true,
		BytesDownloaded: size,
		Path:            req.DestPath,
	})
}

type s3ExportRequest struct {
	SourcePath string `json:"source_path"`
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	Endpoint   string `json:"endpoint,omitempty"`
	AccessKey  string `json:"access_key,omitempty"`
	SecretKey  string `json:"secret_key,omitempty"`
	Region     string `json:"region,omitempty"`
}

type s3ExportResponse struct {
	Success       bool   `json:"success"`
	BytesUploaded int64  `json:"bytes_uploaded"`
	S3Key         string `json:"s3_key"`
}

// Export handles POST /api/v1/sandboxes/{id}/files/s3-export.
func (h *S3Handler) Export(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req s3ExportRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.SourcePath == "" || req.Bucket == "" || req.Key == "" {
		writeError(w, http.StatusBadRequest, "source_path, bucket, and key are required")
		return
	}

	if err := h.validateEndpoint(req.Endpoint); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Validate source_path to prevent path traversal
	if err := pathutil.ValidatePath(req.SourcePath); err != nil {
		writeError(w, http.StatusBadRequest, "invalid source_path: "+err.Error())
		return
	}

	// Read file from sandbox
	data, err := h.engine.ReadFile(r.Context(), id, req.SourcePath)
	if err != nil {
		if errors.Is(err, engine.ErrNotFound) {
			writeError(w, http.StatusNotFound, "sandbox not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to read file from sandbox")
		return
	}
	if int64(len(data)) > maxS3ExportSize {
		writeError(w, http.StatusBadRequest, "file exceeds maximum export size of 100MB")
		return
	}

	// Build S3 config from request + server defaults
	s3Cfg := &runtime.S3SyncConfig{
		Endpoint:  req.Endpoint,
		Bucket:    req.Bucket,
		Region:    req.Region,
		AccessKey: req.AccessKey,
		SecretKey: req.SecretKey,
	}

	creds, err := storage.ResolveS3Credentials(s3Cfg, h.s3Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	client, err := storage.NewS3Client(r.Context(), creds, h.logger)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create S3 client")
		return
	}

	size := int64(len(data))
	if err := client.Upload(r.Context(), req.Bucket, req.Key, bytes.NewReader(data), size); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to upload to S3")
		h.logger.Error("s3 upload failed", "error", err, "bucket", req.Bucket, "key", req.Key)
		return
	}

	writeJSON(w, http.StatusOK, s3ExportResponse{
		Success:       true,
		BytesUploaded: size,
		S3Key:         req.Key,
	})
}
