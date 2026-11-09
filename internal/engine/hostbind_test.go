package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine/enginetest"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

func newHostBindEngine(t *testing.T, policy netpolicy.Policy) (*Engine, *enginetest.MockRuntime) {
	t.Helper()
	st, err := store.NewBoltStore(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	mock := enginetest.NewMockRuntime()
	eng := NewEngine(mock, st, config.SandboxConfig{
		DefaultImage: "test:latest", DefaultTimeout: 10 * time.Minute, MaxSandboxes: 5,
	}, config.S3Config{}, config.DefaultConfig().Resource, policy, slog.Default())
	t.Cleanup(func() { eng.Shutdown(context.Background()) })
	return eng, mock
}

// The runtime mounts what it is handed, so the engine must hand it only
// validated, resolved directories, and refuse everything else before creating.
func TestHostBindsReachTheRuntimeOnlyValidated(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	shared := filepath.Join(root, "shared")
	require.NoError(t, os.Mkdir(shared, 0o755))
	file := filepath.Join(root, "docker.sock")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	outside := t.TempDir()

	ctx := context.Background()
	bind := func(p string) runtime.SandboxConfig {
		return runtime.SandboxConfig{HostBinds: []runtime.HostBind{{HostPath: p, MountPath: "/mnt/x"}}}
	}

	off, _ := newHostBindEngine(t, netpolicy.Policy{})
	_, err = off.CreateSandbox(ctx, bind(shared))
	assert.True(t, errors.Is(err, ErrHostBindRejected), "disabled: %v", err)

	eng, mock := newHostBindEngine(t, netpolicy.Policy{
		HostBindsEnabled: true,
		HostBindRoots:    []netpolicy.HostBindRoot{{Path: root, ReadOnly: true}},
	})
	for _, p := range []string{file, outside} {
		_, err = eng.CreateSandbox(ctx, bind(p))
		assert.True(t, errors.Is(err, ErrHostBindRejected), "%s: %v", p, err)
	}
	assert.Empty(t, mock.Created, "a refused bind must not create anything")

	sb, err := eng.CreateSandbox(ctx, bind(shared+"/."))
	require.NoError(t, err)
	got := mock.Created[sb.ID].HostBinds
	require.Len(t, got, 1)
	assert.Equal(t, runtime.HostBind{HostPath: shared, MountPath: "/mnt/x", ReadOnly: true}, got[0])
}

// A workspace bind whose directory does not exist yet is created, but only
// beneath an allowed root and never through a symlink.
func TestMissingHostBindDirIsCreatedOnlyInsideARoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))

	eng, mock := newHostBindEngine(t, netpolicy.Policy{
		HostBindsEnabled: true,
		HostBindRoots:    []netpolicy.HostBindRoot{{Path: root}},
	})
	ctx := context.Background()
	bind := func(p string) runtime.SandboxConfig {
		return runtime.SandboxConfig{HostBinds: []runtime.HostBind{{HostPath: p, MountPath: "/workspace"}}}
	}

	ws := filepath.Join(root, "projects", "p1", "workspace")
	sb, err := eng.CreateSandbox(ctx, bind(ws))
	require.NoError(t, err)
	fi, err := os.Lstat(ws)
	require.NoError(t, err)
	assert.True(t, fi.IsDir())
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
	assert.Equal(t, ws, mock.Created[sb.ID].HostBinds[0].HostPath)

	for name, p := range map[string]string{
		"through a symlink": filepath.Join(root, "link", "p2"),
		"outside the roots": filepath.Join(outside, "p3", "workspace"),
		"not clean":         root + "/projects/../../escape",
	} {
		_, err := eng.CreateSandbox(ctx, bind(p))
		assert.ErrorIs(t, err, ErrHostBindRejected, name)
	}
	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing may be created outside the root")
	_, err = os.Stat(filepath.Join(filepath.Dir(root), "escape"))
	assert.True(t, os.IsNotExist(err))
}
