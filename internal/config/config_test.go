package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	assert.Equal(t, "0.0.0.0", cfg.Server.Host)
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, "docker", cfg.Runtime.Backend)
	assert.Equal(t, "guino/default:latest", cfg.Sandbox.DefaultImage)
	assert.Equal(t, 30*time.Minute, cfg.Sandbox.DefaultTimeout)
	assert.Equal(t, 100, cfg.Sandbox.MaxSandboxes)
	assert.Equal(t, "den.db", cfg.Store.Path)
	assert.Equal(t, false, cfg.Auth.Enabled)
	assert.Equal(t, "info", cfg.Log.Level)
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yamlContent := `
server:
  host: "127.0.0.1"
  port: 9090
runtime:
  backend: "docker"
sandbox:
  default_image: "custom:latest"
  max_sandboxes: 100
store:
  path: "/tmp/test.db"
auth:
  enabled: true
  api_keys:
    - "key-123"
    - "key-456"
log:
  level: "debug"
  format: "json"
`
	err := os.WriteFile(cfgPath, []byte(yamlContent), 0644)
	require.NoError(t, err)

	cfg, err := Load(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.Server.Host)
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, "custom:latest", cfg.Sandbox.DefaultImage)
	assert.Equal(t, 100, cfg.Sandbox.MaxSandboxes)
	assert.Equal(t, "/tmp/test.db", cfg.Store.Path)
	assert.True(t, cfg.Auth.Enabled)
	assert.Len(t, cfg.Auth.APIKeys, 2)
	assert.Equal(t, "debug", cfg.Log.Level)
	assert.Equal(t, "json", cfg.Log.Format)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("DEN_SERVER__PORT", "7070")
	t.Setenv("DEN_LOG__LEVEL", "warn")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, 7070, cfg.Server.Port)
	assert.Equal(t, "warn", cfg.Log.Level)
}

func TestLoadEmptyPath(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)

	// Should return defaults
	assert.Equal(t, 8080, cfg.Server.Port)
}

func TestLoadInvalidFile(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	assert.Error(t, err)
}

func TestValidate_DefaultNetworkModeEnum(t *testing.T) {
	for _, ok := range []string{"", "internal", "bridge", "none"} {
		c := DefaultConfig()
		c.Runtime.DefaultNetworkMode = ok
		assert.NoError(t, c.Validate(), "mode %q should be valid", ok)
	}
	for _, bad := range []string{"Bridge", "INTERNAL", "host", "garbage", " none"} {
		c := DefaultConfig()
		c.Runtime.DefaultNetworkMode = bad
		err := c.Validate()
		require.Error(t, err, "mode %q should be invalid", bad)
		assert.Contains(t, err.Error(), "default_network_mode")
	}
}

func TestValidate_PlatformOverride(t *testing.T) {
	c := DefaultConfig()
	c.Runtime.PlatformOverride = ""
	assert.NoError(t, c.Validate())

	c = DefaultConfig()
	c.Runtime.PlatformOverride = "linux-native-docker-co-resident"
	assert.NoError(t, c.Validate())

	for _, bad := range []string{"yes", "linux-native", "Linux-Native-Docker-Co-Resident", "true"} {
		c = DefaultConfig()
		c.Runtime.PlatformOverride = bad
		err := c.Validate()
		require.Error(t, err, "override %q should be invalid", bad)
		assert.Contains(t, err.Error(), "platform_override")
	}
}

func TestLoad_DefaultNetworkModeEnvOverride(t *testing.T) {
	t.Setenv("DEN_RUNTIME__DEFAULT_NETWORK_MODE", "none")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "none", cfg.Runtime.DefaultNetworkMode)
}

func TestWarnings(t *testing.T) {
	// docker_host set ⇒ inert advisory.
	c := DefaultConfig()
	c.Runtime.DockerHost = "tcp://somewhere:2375"
	c.Runtime.DefaultNetworkMode = "none" // suppress the internal advisory
	w := c.Warnings()
	require.NotEmpty(t, w)
	joined := ""
	for _, line := range w {
		joined += line + "\n"
	}
	assert.Contains(t, joined, "runtime.docker_host")
	assert.Contains(t, joined, "NO effect")

	// internal (default) ⇒ not-a-boundary advisory.
	c = DefaultConfig()
	c.Runtime.DockerHost = ""
	c.Runtime.DefaultNetworkMode = "" // "" ⇒ internal
	w = c.Warnings()
	require.NotEmpty(t, w)
	assert.Contains(t, w[0], "does NOT contain a sandbox")

	// none ⇒ no advisories at all.
	c = DefaultConfig()
	c.Runtime.DockerHost = ""
	c.Runtime.DefaultNetworkMode = "none"
	assert.Empty(t, c.Warnings())
}
