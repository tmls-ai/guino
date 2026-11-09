package enginetest

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"path"
	"sync"
	"time"

	"github.com/tmls-ai/guino/internal/runtime"
)

// MockRuntime implements runtime.Runtime for testing.
type MockRuntime struct {
	mu      sync.Mutex
	Created map[string]runtime.SandboxConfig
	Started map[string]bool
	Stopped map[string]bool
	Removed map[string]bool
	ExecFn  func(ctx context.Context, id string, opts runtime.ExecOpts) (*runtime.ExecResult, error)
	// IP is reported by Info for every sandbox.
	IP           string
	ExecStreamFn func(ctx context.Context, id string, opts runtime.ExecOpts) (runtime.ExecStream, error)
	// RemoveFn, when set, decides each Remove (failure injection). A nil
	// error records the remove.
	RemoveFn func(ctx context.Context, id string) error
	// CopyFromFn, when set, serves CopyFrom. By default a path is a directory
	// holding one file, "state", whose content names the path.
	CopyFromFn func(id, path string) (io.ReadCloser, error)
	// CopiedTo records the tar stream each CopyTo received, by sandbox id.
	CopiedTo map[string][]byte
	// CopyToFn, when set, replaces CopyTo (it may return without reading).
	CopyToFn func(id string, content io.Reader) error
	// Paused is the pause state per sandbox; PauseCalls and UnpauseCalls count
	// calls. PauseFn, when set, decides each Pause.
	Paused       map[string]bool
	PauseCalls   int
	UnpauseCalls int
	UnpauseFn    func(ctx context.Context, id string) error
	PauseFn      func(id string) error
}

// NewMockRuntime creates a new MockRuntime.
func NewMockRuntime() *MockRuntime {
	return &MockRuntime{
		Created:  make(map[string]runtime.SandboxConfig),
		Started:  make(map[string]bool),
		Stopped:  make(map[string]bool),
		Removed:  make(map[string]bool),
		CopiedTo: make(map[string][]byte),
		Paused:   make(map[string]bool),
	}
}

func (m *MockRuntime) Ping(_ context.Context) error { return nil }

func (m *MockRuntime) Create(_ context.Context, id string, cfg runtime.SandboxConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Created[id] = cfg
	return nil
}

func (m *MockRuntime) Start(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Started[id] = true
	return nil
}

func (m *MockRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Stopped[id] = true
	return nil
}

func (m *MockRuntime) Remove(ctx context.Context, id string) error {
	if m.RemoveFn != nil {
		if err := m.RemoveFn(ctx, id); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Removed[id] = true
	return nil
}

func (m *MockRuntime) Pause(_ context.Context, id string) error {
	if m.PauseFn != nil {
		if err := m.PauseFn(id); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.PauseCalls++
	m.Paused[id] = true
	return nil
}

func (m *MockRuntime) Unpause(ctx context.Context, id string) error {
	if m.UnpauseFn != nil {
		if err := m.UnpauseFn(ctx, id); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.UnpauseCalls++
	m.Paused[id] = false
	return nil
}

// IsPaused reports the pause state (thread-safe).
func (m *MockRuntime) IsPaused(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Paused[id]
}

// Info reports running, or stopped while paused (as Docker's mapping does).
func (m *MockRuntime) Info(_ context.Context, id string) (*runtime.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := runtime.StatusRunning
	if m.Paused[id] {
		status = runtime.StatusStopped
	}
	return &runtime.SandboxInfo{ID: id, Status: status, IP: m.IP, Paused: m.Paused[id]}, nil
}

func (m *MockRuntime) List(_ context.Context) ([]runtime.SandboxInfo, error) {
	return nil, nil
}

func (m *MockRuntime) Exec(ctx context.Context, id string, opts runtime.ExecOpts) (*runtime.ExecResult, error) {
	if m.ExecFn != nil {
		return m.ExecFn(ctx, id, opts)
	}
	return &runtime.ExecResult{ExitCode: 0, Stdout: "ok\n"}, nil
}

func (m *MockRuntime) ExecStream(ctx context.Context, id string, opts runtime.ExecOpts) (runtime.ExecStream, error) {
	if m.ExecStreamFn != nil {
		return m.ExecStreamFn(ctx, id, opts)
	}
	return nil, nil
}

func (m *MockRuntime) ReadFile(_ context.Context, _ string, _ string) ([]byte, error) {
	return []byte("content"), nil
}

func (m *MockRuntime) WriteFile(_ context.Context, _ string, _ string, _ []byte) error {
	return nil
}

func (m *MockRuntime) ListDir(_ context.Context, _ string, _ string) ([]runtime.FileInfo, error) {
	return nil, nil
}

func (m *MockRuntime) Stat(_ context.Context, _ string, _ string) (*runtime.FileInfo, error) {
	return &runtime.FileInfo{}, nil
}

func (m *MockRuntime) MkDir(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *MockRuntime) RemoveFile(_ context.Context, _ string, _ string) error {
	return nil
}

func (m *MockRuntime) Snapshot(_ context.Context, id string, name string) (*runtime.SnapshotInfo, error) {
	return &runtime.SnapshotInfo{ID: "snap-1", SandboxID: id, Name: name}, nil
}

func (m *MockRuntime) Restore(_ context.Context, _ string) (string, error) {
	return "restored-id", nil
}

func (m *MockRuntime) ListSnapshots(_ context.Context, _ string) ([]runtime.SnapshotInfo, error) {
	return nil, nil
}

func (m *MockRuntime) RemoveSnapshot(_ context.Context, _ string) error {
	return nil
}

func (m *MockRuntime) Stats(_ context.Context, _ string) (*runtime.SandboxStats, error) {
	return &runtime.SandboxStats{CPUPercent: 5.0, MemoryUsage: 1024}, nil
}

func (m *MockRuntime) UpdateMemoryLimit(_ context.Context, _ string, _ int64) error {
	return nil
}

func (m *MockRuntime) CopyFrom(_ context.Context, id string, p string) (io.ReadCloser, error) {
	if m.CopyFromFn != nil {
		return m.CopyFromFn(id, p)
	}
	return TarOf(map[string]string{path.Base(p) + "/state": "state of " + p}), nil
}

func (m *MockRuntime) CopyTo(_ context.Context, id string, _ string, content io.Reader) error {
	if m.CopyToFn != nil {
		return m.CopyToFn(id, content)
	}
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CopiedTo[id] = data
	return nil
}

// TarOf builds a tar stream of regular files, name to content.
func TarOf(files map[string]string) io.ReadCloser {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	return io.NopCloser(&buf)
}
