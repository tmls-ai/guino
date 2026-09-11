package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Capture the actual process streams: MCP reserves stdout for JSON-RPC.
func captureMigrationOutput(t *testing.T, run func()) (stdout, stderr string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	require.NoError(t, err)
	defer out.Close()
	errout, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	defer errout.Close()
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errout
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	run()
	outBytes, err := os.ReadFile(out.Name())
	require.NoError(t, err)
	errBytes, err := os.ReadFile(errout.Name())
	require.NoError(t, err)
	return string(outBytes), string(errBytes)
}

func TestMigrationConfigDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		guino      string
		den        string
		explicit   string
		wantPort   int
		wantError  bool
		wantLegacy bool
	}{
		{name: "defaults without files", wantPort: 8080},
		{name: "legacy fallback", den: "server:\n  port: 7070\n", wantPort: 7070, wantLegacy: true},
		{name: "Guino wins", guino: "server:\n  port: 9090\n", den: "server:\n  port: 7070\n", wantPort: 9090},
		{name: "invalid Guino cannot fall back", guino: "server: [", den: "server:\n  port: 7070\n", wantError: true},
		{name: "explicit legacy path wins", guino: "server:\n  port: 9090\n", den: "server:\n  port: 7070\n", explicit: "den.yaml", wantPort: 7070, wantLegacy: true},
		{name: "explicit missing path cannot fall back", den: "server:\n  port: 7070\n", explicit: "missing.yaml", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			for name, content := range map[string]string{"guino.yaml": tc.guino, "den.yaml": tc.den} {
				if content != "" {
					require.NoError(t, os.WriteFile(name, []byte(content), 0600))
				}
			}
			stdout, stderr := captureMigrationOutput(t, func() {
				cfg, err := Load(tc.explicit)
				if tc.wantError {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, tc.wantPort, cfg.Server.Port)
				assert.Equal(t, "den.db", cfg.Store.Path, "the rename must retain existing state")
				assert.Equal(t, "den-net", cfg.Runtime.NetworkID, "the rename must retain existing network ownership")
			})
			assert.Empty(t, stdout)
			assert.Equal(t, tc.wantLegacy, strings.Contains(stderr, "den.yaml is deprecated"))
		})
	}
}

func TestMigrationEnvironmentPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guino.yaml")
	require.NoError(t, os.WriteFile(path, []byte("server:\n  port: 6060\n"), 0600))
	t.Setenv("DEN_SERVER__PORT", "7070")
	t.Setenv("GUINO_SERVER__PORT", "9090")
	t.Setenv("DEN_S3__SECRET_KEY", "legacy-secret-value")
	t.Setenv("GUINO_S3__SECRET_KEY", "")
	t.Setenv("DEN_LOG__LEVEL", "warn")
	t.Setenv("GUINO_LOG__LEVEL", "")
	require.NoError(t, os.Unsetenv("GUINO_LOG__LEVEL"))

	stdout, stderr := captureMigrationOutput(t, func() {
		cfg, err := Load(path)
		require.NoError(t, err)
		assert.Equal(t, 9090, cfg.Server.Port)
		assert.Empty(t, cfg.S3.SecretKey, "explicitly empty Guino value must clear the legacy secret")
		assert.Equal(t, "warn", cfg.Log.Level, "legacy values still override the file/defaults")
	})
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "DEN_LOG__LEVEL is deprecated; use GUINO_LOG__LEVEL")
	assert.NotContains(t, stderr, "legacy-secret-value")
}

func TestMigrationClientEnvironmentAliases(t *testing.T) {
	t.Setenv("DEN_API_KEY", "legacy-secret-value")
	t.Setenv("GUINO_API_KEY", "")
	require.NoError(t, os.Unsetenv("GUINO_API_KEY"))
	stdout, stderr := captureMigrationOutput(t, func() {
		assert.Equal(t, "legacy-secret-value", Env("API_KEY"))
	})
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "DEN_API_KEY is deprecated; use GUINO_API_KEY")
	assert.NotContains(t, stderr, "legacy-secret-value")

	for _, value := range []string{"new-secret-value", ""} {
		t.Setenv("GUINO_API_KEY", value)
		stdout, stderr = captureMigrationOutput(t, func() {
			assert.Equal(t, value, Env("API_KEY"))
		})
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	}
}
