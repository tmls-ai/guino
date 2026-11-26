package engine

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
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

// A container whose process ended on its own (a `kill 1` from inside) is
// started again by the next exec, which then runs; nothing ran twice.
func TestEngine_ExecRevivesExitedContainer(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)
	mock.Started[sb.ID] = false

	mock.Exited[sb.ID] = true
	res, err := eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"echo", "ok"}})
	require.NoError(t, err)
	assert.Equal(t, "ok\n", res.Stdout)
	assert.True(t, mock.Started[sb.ID])
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())

	// Streams and the file calls run on exec too.
	for _, call := range []func() error{
		func() error {
			_, err := eng.ExecStream(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
			return err
		},
		func() error { _, err := eng.ListDir(ctx, sb.ID, "/workspace"); return err },
		func() error { return eng.WriteFile(ctx, sb.ID, "/workspace/a", []byte("a")) },
	} {
		sb.revivedAt = time.Time{} // past the backoff
		mock.Exited[sb.ID] = true
		require.NoError(t, call())
		assert.False(t, mock.Exited[sb.ID])
	}
}

// A container that dies again right after it was started is left stopped,
// so an image that exits at once does not loop.
func TestEngine_ReviveBacksOff(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)

	mock.Exited[sb.ID] = true
	_, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	require.NoError(t, err)

	mock.Exited[sb.ID] = true
	_, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	assert.ErrorIs(t, err, runtime.ErrContainerNotRunning)
	assert.Equal(t, runtime.StatusStopped, sb.GetStatus())
}

// Calls that hit the same dead container together all succeed: one starts it,
// the others wait and find it running.
func TestEngine_ConcurrentRevive(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)

	starts := 0
	mock.StartFn = func(string) error { starts++; time.Sleep(20 * time.Millisecond); return nil }
	mock.Exited[sb.ID] = true
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, 1, starts)
}

// A caller whose request is already cancelled still gets the container
// started; the status never records stopped for a start it cut off.
func TestEngine_ReviveOutlivesCancelledRequest(t *testing.T) {
	eng, mock := newTestEngine(t)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock.StartFn = func(string) error { return nil }
	mock.Exited[sb.ID] = true
	_, _ = eng.Info(ctx, sb.ID)
	assert.False(t, mock.Exited[sb.ID])
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
}

// GET (Info) on a dead container starts it again too, so it never answers
// running for a container that is not.
func TestEngine_InfoRevivesExitedContainer(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)

	mock.Exited[sb.ID] = true
	info, err := eng.Info(ctx, sb.ID)
	require.NoError(t, err)
	assert.Equal(t, runtime.StatusRunning, info.Status)
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
	assert.False(t, mock.Exited[sb.ID])
}

// A container that will not start again is recorded as stopped: the exec
// fails with the runtime's error and GET reports stopped, so the caller can
// replace it.
func TestEngine_ReviveFailureRecordsStopped(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)

	mock.StartFn = func(string) error { return errors.New("start failed") }
	mock.Exited[sb.ID] = true
	_, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	assert.ErrorIs(t, err, runtime.ErrContainerNotRunning)
	assert.Equal(t, runtime.StatusStopped, sb.GetStatus())

	_, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	assert.ErrorIs(t, err, ErrNotRunning)
}

// A paused container (an archive that froze it) is not an exited one.
func TestEngine_InfoLeavesPausedContainer(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)
	mock.Started[sb.ID] = false

	mock.Paused[sb.ID] = true
	_, err = eng.Info(ctx, sb.ID)
	require.NoError(t, err)
	assert.Equal(t, runtime.StatusRunning, sb.GetStatus())
	assert.False(t, mock.Started[sb.ID])
}

// A lifecycle operation in flight owns the container: nothing is started
// under it.
func TestEngine_ReviveLeavesBusySandbox(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)
	mock.Started[sb.ID] = false

	require.NoError(t, sb.claim())
	mock.Exited[sb.ID] = true
	_, err = eng.Info(ctx, sb.ID)
	require.NoError(t, err)
	_, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	assert.ErrorIs(t, err, runtime.ErrContainerNotRunning)
	assert.False(t, mock.Started[sb.ID])
	sb.release()

	_, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	require.NoError(t, err)
	assert.True(t, mock.Started[sb.ID])
}

// A draining host starts nothing: the dead container is recorded stopped, so
// the caller moves the project to another host.
func TestEngine_ReviveRefusedWhileDraining(t *testing.T) {
	eng, mock := newTestEngine(t)
	ctx := context.Background()
	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{Image: "test:latest"})
	require.NoError(t, err)
	mock.Started[sb.ID] = false

	eng.Drain()
	mock.Exited[sb.ID] = true
	_, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"true"}})
	assert.ErrorIs(t, err, runtime.ErrContainerNotRunning)
	assert.False(t, mock.Started[sb.ID])
	assert.Equal(t, runtime.StatusStopped, sb.GetStatus())
}
