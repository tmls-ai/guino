package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Config holds all configuration for the Guino server.
type Config struct {
	Server   ServerConfig   `koanf:"server"`
	Runtime  RuntimeConfig  `koanf:"runtime"`
	Sandbox  SandboxConfig  `koanf:"sandbox"`
	Store    StoreConfig    `koanf:"store"`
	Auth     AuthConfig     `koanf:"auth"`
	Log      LogConfig      `koanf:"log"`
	S3       S3Config       `koanf:"s3"`
	Resource ResourceConfig `koanf:"resource"`
}

// ResourceConfig holds shared resource management settings.
type ResourceConfig struct {
	OvercommitRatio    float64       `koanf:"overcommit_ratio"`
	PressureThreshold  float64       `koanf:"pressure_threshold"`
	CriticalThreshold  float64       `koanf:"critical_threshold"`
	MonitorInterval    time.Duration `koanf:"monitor_interval"`
	EnableAutoThrottle bool          `koanf:"enable_auto_throttle"`
	MinMemoryFloor     int64         `koanf:"min_memory_floor"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host           string   `koanf:"host"`
	Port           int      `koanf:"port"`
	AllowedOrigins []string `koanf:"allowed_origins"`
	RateLimitRPS   float64  `koanf:"rate_limit_rps"`
	RateLimitBurst int      `koanf:"rate_limit_burst"`
	TLS            struct {
		Enabled  bool   `koanf:"enabled"`
		CertFile string `koanf:"cert_file"`
		KeyFile  string `koanf:"key_file"`
	} `koanf:"tls"`
}

// RuntimeConfig holds runtime backend settings.
type RuntimeConfig struct {
	Backend    string `koanf:"backend"` // "docker"
	DockerHost string `koanf:"docker_host"`
	NetworkID  string `koanf:"network_id"`

	// DefaultNetworkMode is the global default sandbox network mode:
	// "internal" (default), "bridge", or "none". Empty is treated as
	// "internal" for backward compatibility.
	DefaultNetworkMode string `koanf:"default_network_mode"`
	// ReconcileNetwork enables operator-initiated, spoof-resistant
	// destruction+recreation of the managed network when its mode changed.
	ReconcileNetwork bool `koanf:"reconcile_network"`
	// AllowUnsafeBridge must be true to start with default_network_mode=bridge
	// (NAT'd, unfiltered egress; no egress filter in v1).
	AllowUnsafeBridge bool `koanf:"allow_unsafe_bridge"`
	// AllowUnsafeBind is the dangerous last-resort opt-in that disables the
	// bind guard entirely (exposes the unauthenticated control plane).
	AllowUnsafeBind bool `koanf:"allow_unsafe_bind"`
	// PlatformOverride, when set to the single literal
	// "linux-native-docker-co-resident", is the operator's MANDATORY
	// co-residency attestation that unlocks the bind guard's loopback branch.
	// Any other non-empty value is a config error.
	PlatformOverride string `koanf:"platform_override"`
}

// TmpfsDefault defines a default tmpfs mount.
type TmpfsDefault struct {
	Path string `koanf:"path"`
	Size string `koanf:"size"`
}

// SandboxConfig holds default sandbox settings.
type SandboxConfig struct {
	DefaultImage         string         `koanf:"default_image"`
	DefaultTimeout       time.Duration  `koanf:"default_timeout"`
	MaxSandboxes         int            `koanf:"max_sandboxes"`
	DefaultCPU           int64          `koanf:"default_cpu"`    // NanoCPUs
	DefaultMemory        int64          `koanf:"default_memory"` // bytes
	DefaultPidLimit      int64          `koanf:"default_pid_limit"`
	WarmPoolSize         int            `koanf:"warm_pool_size"`
	AllowVolumes         bool           `koanf:"allow_volumes"`
	AllowSharedVolumes   bool           `koanf:"allow_shared_volumes"`
	AllowS3              bool           `koanf:"allow_s3"`
	AllowS3FUSE          bool           `koanf:"allow_s3_fuse"`
	AllowHostBinds       bool           `koanf:"allow_host_binds"`
	MaxVolumesPerSandbox int            `koanf:"max_volumes_per_sandbox"`
	DefaultTmpfs         []TmpfsDefault `koanf:"default_tmpfs"`
}

// S3Config holds server-wide S3 defaults.
type S3Config struct {
	Endpoint  string `koanf:"endpoint"`
	Region    string `koanf:"region"`
	AccessKey string `koanf:"access_key"`
	SecretKey string `koanf:"secret_key"`
	// AllowInternalEndpoint opts the single configured S3 endpoint back into
	// loopback/RFC1918/CGNAT reachability (self-hosted MinIO), pinned to its
	// construction-time IP set. Default false: the SSRF guard blocks all
	// internal ranges. Metadata/link-local/multicast/unspecified are NEVER
	// reachable regardless of this flag. Env: GUINO_S3__ALLOW_INTERNAL_ENDPOINT.
	AllowInternalEndpoint bool `koanf:"allow_internal_endpoint"`
}

// maskSecret renders a secret as a fixed redaction token so String() can never
// leak the literal. Empty is reported distinctly so a missing key is
// operator-diagnosable.
func maskSecret(s string) string {
	if s == "" {
		return "(empty)"
	}
	return "***"
}

// String returns a safe representation of S3Config. BOTH AccessKey and
// SecretKey are masked — the configreflection test asserts, per field, that
// the field name appears here and that neither secret's literal does.
func (c S3Config) String() string {
	return fmt.Sprintf(
		"S3Config{Endpoint:%s Region:%s AccessKey:%s SecretKey:%s AllowInternalEndpoint:%t}",
		c.Endpoint, c.Region, maskSecret(c.AccessKey), maskSecret(c.SecretKey), c.AllowInternalEndpoint)
}

// StoreConfig holds state persistence settings.
type StoreConfig struct {
	Path string `koanf:"path"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	APIKeys []string `koanf:"api_keys"`
	Enabled bool     `koanf:"enabled"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level  string `koanf:"level"`  // "debug", "info", "warn", "error"
	Format string `koanf:"format"` // "text", "json"
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:           "0.0.0.0",
			Port:           8080,
			AllowedOrigins: []string{"http://localhost:8080", "http://127.0.0.1:8080"},
			RateLimitRPS:   10,
			RateLimitBurst: 20,
		},
		Runtime: RuntimeConfig{
			Backend: "docker",
			// Preserve the persisted network identity across upgrades.
			NetworkID:          "den-net",
			DefaultNetworkMode: "internal",
		},
		Sandbox: SandboxConfig{
			DefaultImage:         "guino/default:latest",
			DefaultTimeout:       30 * time.Minute,
			MaxSandboxes:         100,
			DefaultCPU:           0, // 0 = no limit, shared across all containers
			DefaultMemory:        0, // 0 = no limit, all containers share host RAM
			DefaultPidLimit:      256,
			WarmPoolSize:         0,
			AllowVolumes:         true,
			AllowSharedVolumes:   true,
			AllowS3:              true,
			AllowS3FUSE:          false,
			AllowHostBinds:       false,
			MaxVolumesPerSandbox: 5,
			DefaultTmpfs: []TmpfsDefault{
				{Path: "/tmp", Size: "256m"},
				{Path: "/home/sandbox", Size: "512m"},
				{Path: "/run", Size: "64m"},
				{Path: "/var/tmp", Size: "128m"},
			},
		},
		S3: S3Config{
			Region: "us-east-1",
		},
		Store: StoreConfig{
			// Keep existing state discoverable; do not create an empty Guino database.
			Path: "den.db",
		},
		Auth: AuthConfig{
			Enabled: false,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
		Resource: ResourceConfig{
			OvercommitRatio:    10.0,
			PressureThreshold:  0.80,
			CriticalThreshold:  0.90,
			MonitorInterval:    5 * time.Second,
			EnableAutoThrottle: true,
			MinMemoryFloor:     32 * 1024 * 1024, // 32MB
		},
	}
}

// Validate checks the config for obvious misconfigurations.
func (c *Config) Validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid server port: %d", c.Server.Port)
	}
	if c.Sandbox.MaxSandboxes <= 0 {
		return fmt.Errorf("max_sandboxes must be positive")
	}
	if c.Sandbox.DefaultTimeout <= 0 {
		return fmt.Errorf("default_timeout must be positive")
	}
	if c.Sandbox.DefaultImage == "" {
		return fmt.Errorf("default_image is required")
	}
	if c.Sandbox.MaxVolumesPerSandbox < 0 {
		return fmt.Errorf("max_volumes_per_sandbox must be non-negative")
	}
	if c.Sandbox.WarmPoolSize < 0 {
		return fmt.Errorf("warm_pool_size must be non-negative")
	}
	if c.Auth.Enabled && len(c.Auth.APIKeys) == 0 {
		return fmt.Errorf("auth is enabled but no api_keys configured")
	}
	if c.Server.TLS.Enabled {
		if c.Server.TLS.CertFile == "" || c.Server.TLS.KeyFile == "" {
			return fmt.Errorf("TLS is enabled but cert_file or key_file is missing")
		}
	}
	if c.Sandbox.DefaultMemory > 0 && c.Sandbox.DefaultMemory < 4*1024*1024 {
		return fmt.Errorf("default_memory must be at least 4MB")
	}

	// Resource config validation
	if c.Resource.PressureThreshold >= c.Resource.CriticalThreshold {
		return fmt.Errorf("pressure_threshold must be less than critical_threshold")
	}
	if c.Resource.CriticalThreshold >= 1.0 {
		return fmt.Errorf("critical_threshold must be less than 1.0")
	}
	if c.Resource.MinMemoryFloor < 4*1024*1024 {
		return fmt.Errorf("min_memory_floor must be at least 4MB")
	}
	if c.Resource.MonitorInterval < 1*time.Second {
		return fmt.Errorf("monitor_interval must be at least 1s")
	}

	// Network mode enum. "" is accepted and treated as "internal".
	switch c.Runtime.DefaultNetworkMode {
	case "", "internal", "bridge", "none":
		// ok
	default:
		return fmt.Errorf("invalid runtime.default_network_mode %q: must be internal, bridge, or none", c.Runtime.DefaultNetworkMode)
	}

	// platform_override accepts exactly "" or the single co-residency
	// attestation literal, case-sensitive. Keep this literal in sync with
	// netpolicy.PlatformOverrideCoResident (config stays a stdlib leaf, so
	// the constant is duplicated here intentionally).
	switch c.Runtime.PlatformOverride {
	case "", "linux-native-docker-co-resident":
		// ok
	default:
		return fmt.Errorf("invalid runtime.platform_override %q: must be empty or \"linux-native-docker-co-resident\"", c.Runtime.PlatformOverride)
	}

	// The SSRF exemption is meaningless without a concrete endpoint to pin.
	// Reject allow_internal_endpoint=true with an empty/host-less endpoint so
	// the operator cannot silently widen the guard to "any internal host".
	if c.S3.AllowInternalEndpoint {
		if _, err := extractEndpointHost(c.S3.Endpoint); err != nil {
			return fmt.Errorf(
				"storage.s3.allow_internal_endpoint=true requires storage.s3.endpoint "+
					"to carry an extractable host: %w", err)
		}
	}

	return nil
}

// extractEndpointHost pulls a host out of a configured S3 endpoint using
// stdlib only (config must stay an import leaf; it deliberately does NOT
// import internal/security/ssrf). It is back-compat-safe: a full URL, a bare
// host:port, and a bare hostname/IP with no port are all accepted — today's
// configs are unvalidated and may be scheme-less. The rule is only that a
// non-empty host is extractable; it mandates neither a scheme nor a port.
func extractEndpointHost(endpoint string) (string, error) {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return "", fmt.Errorf("endpoint is empty")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("unparseable endpoint URL: %w", err)
		}
		if u.Hostname() == "" {
			return "", fmt.Errorf("endpoint URL has no host")
		}
		return u.Hostname(), nil
	}
	if h, _, err := net.SplitHostPort(raw); err == nil && h != "" {
		return h, nil
	}
	// Bare hostname/IP with no port (net.SplitHostPort errors "missing port").
	return raw, nil
}

// Warnings returns non-fatal configuration advisories. It never fails a
// startup; callers log each line at WARN. Security-critical disclosures
// (allow_unsafe_bind / platform_override attestation) are logged at ERROR by
// the guard wiring, not here.
func (c *Config) Warnings() []string {
	var w []string
	if c.Runtime.DockerHost != "" {
		w = append(w, "runtime.docker_host is set but has NO effect; the Docker endpoint is controlled solely by the DOCKER_HOST environment variable")
	}
	mode := c.Runtime.DefaultNetworkMode
	if mode == "" {
		mode = "internal"
	}
	if mode == "internal" {
		w = append(w, "network_mode=internal does NOT contain a sandbox: it still reaches the bridge gateway, the embedded DNS resolver and any host service bound to 0.0.0.0. Only network_mode=none is a tenant/egress boundary in v1")
	}
	return w
}

// Env reads a Guino environment variable by suffix (for example, "API_KEY").
// DEN_ aliases remain supported through 0.1.x. An explicitly set GUINO_ value,
// including an empty value, always wins. Warnings never include secret values.
func Env(suffix string) string {
	if value, ok := os.LookupEnv("GUINO_" + suffix); ok {
		return value
	}
	if value, ok := os.LookupEnv("DEN_" + suffix); ok {
		warnLegacyEnv("DEN_" + suffix)
		return value
	}
	return ""
}

func warnLegacyEnv(name string) {
	_, _ = fmt.Fprintf(os.Stderr, "Warning: %s is deprecated; use GUINO_%s. DEN_ aliases are supported through 0.1.x.\n", name, strings.TrimPrefix(name, "DEN_"))
}

// Load reads a YAML file and environment variables. With no explicit path,
// guino.yaml is preferred over the deprecated den.yaml fallback. GUINO_ variables
// override DEN_ aliases, which override file settings. Use __ as the nesting
// separator, for example GUINO_SERVER__PORT=9090. Compatibility warnings go to
// stderr so loading configuration cannot corrupt an MCP stdio stream.
func Load(path string) (*Config, error) {
	k := koanf.New(".")
	cfg := DefaultConfig()

	if path == "" {
		for _, candidate := range []string{"guino.yaml", "den.yaml"} {
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
				break
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("checking config file %s: %w", candidate, err)
			}
		}
	}

	// Load from YAML file if provided
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return nil, fmt.Errorf("loading config file %s: %w", path, err)
		}
		if filepath.Base(path) == "den.yaml" {
			_, _ = fmt.Fprintln(os.Stderr, "Warning: den.yaml is deprecated; rename it to guino.yaml. The fallback is supported through 0.1.x.")
		}
	}

	// The second overlay wins even when its value is explicitly empty.
	for _, prefix := range []string{"DEN_", "GUINO_"} {
		if err := k.Load(env.Provider(prefix, ".", func(name string) string {
			if prefix == "DEN_" {
				if _, present := os.LookupEnv("GUINO_" + strings.TrimPrefix(name, "DEN_")); !present {
					warnLegacyEnv(name)
				}
			}
			return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(name, prefix)), "__", ".")
		}), nil); err != nil {
			return nil, fmt.Errorf("loading env config: %w", err)
		}
	}

	if err := k.Unmarshal("", cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling config: %w", err)
	}

	// Validate is wired into the load path so structural guards (notably the
	// allow_internal_endpoint SSRF guard) cannot be skipped by a caller that
	// forgets to call Validate(). Idempotent; entrypoints may call it again.
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}
