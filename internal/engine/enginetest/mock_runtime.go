package enginetest

import (
	"context"
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
}

// NewMockRuntime creates a new MockRuntime.
func NewMockRuntime() *MockRuntime {
	return &MockRuntime{
		Created: make(map[string]runtime.SandboxConfig),
		Started: make(map[string]bool),
		Stopped: make(map[string]bool),
		Removed: make(map[string]bool),
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

func (m *MockRuntime) Remove(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Removed[id] = true
	return nil
}

func (m *MockRuntime) Info(_ context.Context, id string) (*runtime.SandboxInfo, error) {
	return &runtime.SandboxInfo{ID: id, Status: runtime.StatusRunning}, nil
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

func (m *MockRuntime) ExecStream(_ context.Context, _ string, _ runtime.ExecOpts) (runtime.ExecStream, error) {
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
