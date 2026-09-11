package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Client is the Go client for the Guino API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client

	// features caches the server capability tokens after the first
	// successful /api/v1/version probe. nil until then so a transient
	// probe failure is retried rather than cached.
	featMu   sync.Mutex
	features map[string]bool
}

// Option configures the Client.
type Option func(*Client)

// WithAPIKey sets the API key for authentication.
func WithAPIKey(key string) Option {
	return func(c *Client) {
		c.apiKey = key
	}
}

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// New creates a new Guino client.
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// PortMapping defines a port forwarding between host and sandbox.
type PortMapping struct {
	SandboxPort int `json:"sandbox_port"`
	HostPort    int `json:"host_port"`
	// Protocol is always "tcp"; Guino does not support udp port publishing.
	Protocol string `json:"protocol,omitempty"`
}

// VolumeMount defines a named volume to mount into a sandbox.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// TmpfsMount defines a tmpfs filesystem to mount inside a sandbox.
type TmpfsMount struct {
	Path    string `json:"path"`
	Size    string `json:"size"`
	Options string `json:"options,omitempty"`
}

// S3SyncConfig holds S3 synchronization settings for a sandbox.
type S3SyncConfig struct {
	Endpoint  string `json:"endpoint,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix,omitempty"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`
	Mode      string `json:"mode"`
	MountPath string `json:"mount_path,omitempty"`
	SyncPath  string `json:"sync_path,omitempty"`
}

// StorageConfig holds storage settings for a sandbox.
type StorageConfig struct {
	Volumes []VolumeMount `json:"volumes,omitempty"`
	Tmpfs   []TmpfsMount  `json:"tmpfs,omitempty"`
	S3      *S3SyncConfig `json:"s3,omitempty"`
}

// SandboxConfig holds sandbox creation options.
type SandboxConfig struct {
	Image    string            `json:"image,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	WorkDir  string            `json:"workdir,omitempty"`
	Timeout  int               `json:"timeout,omitempty"` // seconds
	CPU      int64             `json:"cpu,omitempty"`     // NanoCPUs (1e9 = 1 core)
	Memory   int64             `json:"memory,omitempty"`  // bytes
	PidLimit int64             `json:"pid_limit,omitempty"`
	Ports    []PortMapping     `json:"ports,omitempty"`
	Storage  *StorageConfig    `json:"storage,omitempty"`
	// NetworkMode is the per-sandbox network override. Only "" (inherit the
	// server default) or "none" (no network) are accepted; any other value
	// — including one equal to the server default — is rejected with HTTP
	// 400. A per-sandbox value may only INCREASE isolation.
	NetworkMode string `json:"network_mode,omitempty"`
}

// Sandbox represents a sandbox instance.
type Sandbox struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Ports echoes the resolved port mappings. Populated only in bridge
	// mode; empty in internal/none (publishing is inert without egress).
	Ports []PortMapping `json:"ports,omitempty"`
}

// ExecResult holds the result of command execution.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// SnapshotInfo holds snapshot metadata.
type SnapshotInfo struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// requireFeature fails fast if the server does not advertise feat. It is a
// capability hint only — NOT auth. The probe is lazy and scoped: callers
// invoke it only when the corresponding option is actually used, so servers
// that predate a feature keep working for everything else. The result is
// cached only on success.
func (c *Client) requireFeature(ctx context.Context, feat string) error {
	c.featMu.Lock()
	defer c.featMu.Unlock()
	if c.features == nil {
		var v struct {
			Features []string `json:"features"`
		}
		if err := c.get(ctx, "/api/v1/version", &v); err != nil {
			return fmt.Errorf("checking server %q support: %w", feat, err)
		}
		c.features = make(map[string]bool, len(v.Features))
		for _, f := range v.Features {
			c.features[f] = true
		}
	}
	if !c.features[feat] {
		return fmt.Errorf("server does not advertise the %q feature; upgrade Guino or omit network_mode", feat)
	}
	return nil
}

// CreateSandbox creates a new sandbox.
func (c *Client) CreateSandbox(ctx context.Context, cfg SandboxConfig) (*Sandbox, error) {
	if cfg.NetworkMode != "" {
		if err := c.requireFeature(ctx, "network_mode"); err != nil {
			return nil, err
		}
	}
	var sb Sandbox
	if err := c.post(ctx, "/api/v1/sandboxes", cfg, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// GetSandbox returns a sandbox by ID.
func (c *Client) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	var sb Sandbox
	if err := c.get(ctx, "/api/v1/sandboxes/"+id, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// ListSandboxes returns all sandboxes.
func (c *Client) ListSandboxes(ctx context.Context) ([]Sandbox, error) {
	var sbs []Sandbox
	if err := c.get(ctx, "/api/v1/sandboxes", &sbs); err != nil {
		return nil, err
	}
	return sbs, nil
}

// DestroySandbox removes a sandbox.
func (c *Client) DestroySandbox(ctx context.Context, id string) error {
	return c.delete(ctx, "/api/v1/sandboxes/"+id)
}

// StopSandbox stops a sandbox.
func (c *Client) StopSandbox(ctx context.Context, id string) error {
	return c.post(ctx, "/api/v1/sandboxes/"+id+"/stop", nil, nil)
}

// ExecOpts holds options for command execution.
type ExecOpts struct {
	Cmd     []string          `json:"cmd"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"workdir,omitempty"`
	Timeout int               `json:"timeout,omitempty"` // seconds
}

// Exec runs a command in a sandbox.
func (c *Client) Exec(ctx context.Context, id string, opts ExecOpts) (*ExecResult, error) {
	var result ExecResult
	if err := c.post(ctx, "/api/v1/sandboxes/"+id+"/exec", opts, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ReadFile reads a file from a sandbox.
func (c *Client) ReadFile(ctx context.Context, id string, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/api/v1/sandboxes/"+id+"/files?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	return io.ReadAll(resp.Body)
}

// WriteFile writes a file to a sandbox.
func (c *Client) WriteFile(ctx context.Context, id string, path string, content []byte) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", c.baseURL+"/api/v1/sandboxes/"+id+"/files?path="+url.QueryEscape(path), bytes.NewReader(content))
	if err != nil {
		return err
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return c.parseError(resp)
	}
	return nil
}

// CreateSnapshot creates a snapshot of a sandbox.
func (c *Client) CreateSnapshot(ctx context.Context, id string, name string) (*SnapshotInfo, error) {
	var snap SnapshotInfo
	if err := c.post(ctx, "/api/v1/sandboxes/"+id+"/snapshots", map[string]string{"name": name}, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// RestoreSnapshot restores a sandbox from a snapshot.
func (c *Client) RestoreSnapshot(ctx context.Context, snapshotID string) (*Sandbox, error) {
	var sb Sandbox
	if err := c.post(ctx, "/api/v1/snapshots/"+snapshotID+"/restore", nil, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// Health checks server health.
func (c *Client) Health(ctx context.Context) error {
	return c.get(ctx, "/api/v1/health", nil)
}

func (c *Client) get(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	return c.do(req, result)
}

func (c *Client) post(ctx context.Context, path string, body any, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	c.setHeaders(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.do(req, result)
}

func (c *Client) delete(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, "DELETE", c.baseURL+path, nil)
	if err != nil {
		return err
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}
	return nil
}

func (c *Client) do(req *http.Request, result any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

func (c *Client) setHeaders(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
}

func (c *Client) parseError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		return fmt.Errorf("API error (%d): %s", resp.StatusCode, errResp.Error)
	}
	return fmt.Errorf("API error (%d): %s", resp.StatusCode, string(body))
}
