package engine

import (
	"context"
	"log/slog"
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

func newTestEngine(t *testing.T) (*Engine, *enginetest.MockRuntime) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewBoltStore(filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	mock := enginetest.NewMockRuntime()
	cfg := config.SandboxConfig{
		DefaultImage:    "test:latest",
		DefaultTimeout:  10 * time.Minute,
		MaxSandboxes:    5,
		DefaultCPU:      1_000_000_000,
		DefaultMemory:   512 * 1024 * 1024,
		DefaultPidLimit: 256,
	}

	eng := NewEngine(mock, st, cfg, config.S3Config{}, config.DefaultConfig().Resource, netpolicy.Policy{}, slog.Default())
	t.Cleanup(func() {
		eng.Shutdown(context.Background())
	})

	return eng, mock
}

func TestEngine_CreateSandbox(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()

	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{
		Image: "ubuntu:24.04",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, sb.ID)
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
	assert.Equal(t, "ubuntu:24.04", sb.Image)

	assert.Contains(t, mock.Created, sb.ID)
	assert.True(t, mock.Started[sb.ID])
}

func TestEngine_CreateSandbox_DefaultImage(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()

	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)

	assert.Equal(t, "test:latest", mock.Created[sb.ID].Image)
}

func TestEngine_CreateSandbox_LimitReached(t *testing.T) {
	eng, _ := newTestEngine(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
		require.NoError(t, err)
	}

	_, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	assert.ErrorIs(t, err, ErrLimitReached)
}

func TestEngine_GetSandbox(t *testing.T) {
	eng, _ := newTestEngine(t)
	ctx := context.Background()

	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)

	got, err := eng.GetSandbox(sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.ID, got.ID)
}

func TestEngine_GetSandbox_NotFound(t *testing.T) {
	eng, _ := newTestEngine(t)

	_, err := eng.GetSandbox("nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestEngine_ListSandboxes(t *testing.T) {
	eng, _ := newTestEngine(t)
	ctx := context.Background()

	eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	eng.CreateSandbox(ctx, runtime.SandboxConfig{})

	list := eng.ListSandboxes()
	assert.Len(t, list, 2)
}

func TestEngine_DestroySandbox(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()

	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)

	err = eng.DestroySandbox(ctx, sb.ID)
	require.NoError(t, err)

	assert.True(t, mock.Removed[sb.ID])

	_, err = eng.GetSandbox(sb.ID)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestEngine_DestroySandbox_NotFound(t *testing.T) {
	eng, _ := newTestEngine(t)

	err := eng.DestroySandbox(context.Background(), "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestEngine_Exec(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()

	mock.ExecFn = func(_ context.Context, _ string, _ runtime.ExecOpts) (*runtime.ExecResult, error) {
		return &runtime.ExecResult{ExitCode: 0, Stdout: "hello\n"}, nil
	}

	sb, _ := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	result, err := eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"echo", "hello"}})
	require.NoError(t, err)
	assert.Equal(t, 0, result.ExitCode)
	assert.Equal(t, "hello\n", result.Stdout)
}

func TestEngine_Exec_NotFound(t *testing.T) {
	eng, _ := newTestEngine(t)

	_, err := eng.Exec(context.Background(), "nonexistent", runtime.ExecOpts{Cmd: []string{"echo"}})
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestEngine_RestoreSnapshot_LimitReached(t *testing.T) {
	eng, _ := newTestEngine(t)
	ctx := context.Background()

	// Fill up to max
	for i := 0; i < 5; i++ {
		_, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
		require.NoError(t, err)
	}

	// Restore should fail with limit reached
	_, err := eng.RestoreSnapshot(ctx, "some-snapshot")
	assert.ErrorIs(t, err, ErrLimitReached)
}

func TestSandbox_SetStatus_ThreadSafe(t *testing.T) {
	sb := &Sandbox{
		ID:     "test",
		status: runtime.StatusCreating,
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			sb.SetStatus(runtime.StatusRunning)
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		sb.GetStatus()
	}
	<-done
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
}
