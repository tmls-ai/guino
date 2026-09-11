package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/xid"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/storage"
	"github.com/tmls-ai/guino/internal/store"
)

var (
	ErrNotFound        = errors.New("sandbox not found")
	ErrNotRunning      = errors.New("sandbox is not running")
	ErrLimitReached    = errors.New("maximum sandbox limit reached")
	ErrPressureTooHigh = errors.New("host under critical memory pressure")
)

// Engine orchestrates sandbox lifecycle.
type Engine struct {
	runtime         runtime.Runtime
	store           store.Store
	config          config.SandboxConfig
	s3Config        config.S3Config
	resourceConfig  config.ResourceConfig
	netPolicy       netpolicy.Policy
	sandboxes       sync.Map // map[string]*Sandbox
	mu              sync.Mutex
	count           int
	logger          *slog.Logger
	stopCh          chan struct{}
	shutdownOnce    sync.Once
	pressureMonitor *PressureMonitor
	pressureCh      chan PressureEvent
}

// NewEngine creates a new Engine.
func NewEngine(rt runtime.Runtime, st store.Store, cfg config.SandboxConfig, s3Cfg config.S3Config, resCfg config.ResourceConfig, policy netpolicy.Policy, logger *slog.Logger) *Engine {
	e := &Engine{
		runtime:        rt,
		store:          st,
		config:         cfg,
		s3Config:       s3Cfg,
		resourceConfig: resCfg,
		netPolicy:      policy,
		logger:         logger,
		stopCh:         make(chan struct{}),
		pressureCh:     make(chan PressureEvent, 16),
	}

	// Start pressure monitor with config-driven thresholds
	backend := NewPlatformMemoryBackend()
	thresholds := PressureThresholds{
		Warning:   resCfg.PressureThreshold - 0.10, // One level below pressure threshold
		High:      resCfg.PressureThreshold,
		Critical:  resCfg.CriticalThreshold,
		Emergency: resCfg.CriticalThreshold + 0.05,
	}
	e.pressureMonitor = NewPressureMonitor(backend, resCfg.MonitorInterval, thresholds, logger)
	e.pressureMonitor.Subscribe(e.pressureCh)
	e.pressureMonitor.Start()
	go e.handlePressureEvents()

	// Restore sandboxes from store
	e.restoreFromStore()

	// Start reaper goroutine
	go e.reaper()

	return e
}

// SandboxCount returns total and running sandbox counts without allocating a slice.
func (e *Engine) SandboxCount() (total, running int) {
	e.sandboxes.Range(func(_, v any) bool {
		total++
		if v.(*Sandbox).GetStatus() == runtime.StatusRunning {
			running++
		}
		return true
	})
	return
}

// CurrentPressure returns the current pressure event.
func (e *Engine) CurrentPressure() PressureEvent {
	return e.pressureMonitor.CurrentEvent()
}

// PressureThresholds returns the configured pressure thresholds.
func (e *Engine) PressureThresholds() PressureThresholds {
	return e.pressureMonitor.thresholds
}

// handlePressureEvents processes pressure level changes from the monitor.
func (e *Engine) handlePressureEvents() {
	for {
		select {
		case evt, ok := <-e.pressureCh:
			if !ok {
				return
			}
			e.onPressureChange(evt)
		case <-e.stopCh:
			return
		}
	}
}

func (e *Engine) onPressureChange(evt PressureEvent) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.Error("panic in pressure handler, recovered", "panic", r)
		}
	}()

	switch evt.Level {
	case PressureNormal, PressureWarning:
		if e.resourceConfig.EnableAutoThrottle {
			e.logger.Info("memory pressure eased, removing container memory limits",
				"level", evt.Level.String(), "score", evt.Score)
			e.removeContainerMemoryLimits()
		}
	case PressureHigh:
		if e.resourceConfig.EnableAutoThrottle {
			e.logger.Warn("high memory pressure, updating container memory limits",
				"score", evt.Score)
			e.updateContainerMemoryLimits(evt)
		}
	case PressureCritical, PressureEmergency:
		e.logger.Error("critical memory pressure",
			"level", evt.Level.String(),
			"score", evt.Score,
		)
		if e.resourceConfig.EnableAutoThrottle {
			e.updateContainerMemoryLimits(evt)
		}
	}
}

// removeContainerMemoryLimits restores unlimited memory for all running containers.
func (e *Engine) removeContainerMemoryLimits() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.sandboxes.Range(func(_, v any) bool {
		sb := v.(*Sandbox)
		if sb.GetStatus() == runtime.StatusRunning {
			// 0 means remove the limit (unlimited)
			if err := e.runtime.UpdateMemoryLimit(ctx, sb.ID, 0); err != nil {
				e.logger.Warn("failed to remove memory limit", "id", sb.ID, "error", err)
			}
		}
		return true
	})
}

// memoryHighSafetyFactor is the fraction of free memory to distribute among containers.
// 0.8 means retain 20% headroom for host/system processes.
const memoryHighSafetyFactor = 0.8

func (e *Engine) updateContainerMemoryLimits(evt PressureEvent) {
	// Single Range pass: collect running sandbox IDs for consistent snapshot
	var targets []string
	e.sandboxes.Range(func(_, v any) bool {
		sb := v.(*Sandbox)
		if sb.GetStatus() == runtime.StatusRunning {
			targets = append(targets, sb.ID)
		}
		return true
	})

	if len(targets) == 0 {
		return
	}

	// Calculate memory.high per container with underflow guard
	var freeMemory uint64
	if evt.MemoryTotal > evt.MemoryUsed {
		freeMemory = evt.MemoryTotal - evt.MemoryUsed
	}
	memoryHigh := int64(float64(freeMemory)*memoryHighSafetyFactor) / int64(len(targets))
	minFloor := e.resourceConfig.MinMemoryFloor
	if memoryHigh < minFloor {
		memoryHigh = minFloor
	}

	e.logger.Info("adjusting container memory limits",
		"memory_high", memoryHigh,
		"active_containers", len(targets),
	)

	// Apply memory limits to collected targets
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range targets {
		if err := e.runtime.UpdateMemoryLimit(ctx, id, memoryHigh); err != nil {
			e.logger.Warn("failed to update memory limit", "id", id, "error", err)
		}
	}
}

// CreateSandbox creates and starts a new sandbox.
func (e *Engine) CreateSandbox(ctx context.Context, cfg runtime.SandboxConfig) (*Sandbox, error) {
	// Single network-policy chokepoint: validates the per-sandbox request,
	// resolves the effective mode onto cfg.NetworkMode, normalizes ports and
	// strips den.* labels. Returns a *netpolicy.ValidationError un-wrapped so
	// the HTTP/MCP handlers can map it to a stable 400.
	if err := e.netPolicy.ResolveAndValidate(&cfg); err != nil {
		return nil, err
	}

	e.mu.Lock()
	// Check pressure under lock to prevent TOCTOU race
	if pressure := e.pressureMonitor.CurrentEvent(); pressure.Level >= PressureCritical {
		e.mu.Unlock()
		return nil, ErrPressureTooHigh
	}
	if e.count >= e.config.MaxSandboxes {
		e.mu.Unlock()
		return nil, ErrLimitReached
	}
	// Validate storage under mutex to prevent TOCTOU race on shared volume checks
	if err := e.validateStorage(cfg.Storage); err != nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("invalid storage config: %w", err)
	}
	e.count++
	e.mu.Unlock()

	// Apply defaults
	if cfg.Image == "" {
		cfg.Image = e.config.DefaultImage
	}
	if cfg.CPU == 0 {
		cfg.CPU = e.config.DefaultCPU
	}
	if cfg.Memory == 0 {
		cfg.Memory = e.config.DefaultMemory
	}
	if cfg.PidLimit == 0 {
		cfg.PidLimit = e.config.DefaultPidLimit
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = e.config.DefaultTimeout
	}

	// Build tmpfs map from defaults + per-sandbox overrides
	tmpfsMap, err := storage.BuildTmpfsMap(cfg.Storage, e.config.DefaultTmpfs)
	if err != nil {
		e.decrementCount()
		return nil, fmt.Errorf("building tmpfs config: %w", err)
	}
	cfg.TmpfsMap = tmpfsMap

	id := xid.New().String()

	sandbox := &Sandbox{
		ID:        id,
		Image:     cfg.Image,
		status:    runtime.StatusCreating,
		Config:    cfg,
		Ports:     cfg.Ports,
		CreatedAt: time.Now().UTC(),
	}
	if timeout > 0 {
		sandbox.ExpiresAt = sandbox.CreatedAt.Add(timeout)
	}

	// Create container
	if err := e.runtime.Create(ctx, id, cfg); err != nil {
		e.decrementCount()
		return nil, fmt.Errorf("creating sandbox: %w", err)
	}

	// Start container
	if err := e.runtime.Start(ctx, id); err != nil {
		_ = e.runtime.Remove(ctx, id)
		e.decrementCount()
		return nil, fmt.Errorf("starting sandbox: %w", err)
	}

	sandbox.SetStatus(runtime.StatusRunning)

	// Store sandbox before hooks so it's discoverable during S3 init
	e.sandboxes.Store(id, sandbox)
	if err := e.saveSandbox(sandbox); err != nil {
		e.logger.Warn("failed to save sandbox to store", "id", id, "error", err)
	}

	// S3 hooks: download files after container start
	if cfg.Storage != nil && cfg.Storage.S3 != nil && cfg.Storage.S3.Mode == runtime.S3SyncModeHooks {
		if err := e.s3HookInit(ctx, id, cfg.Storage.S3); err != nil {
			e.logger.Warn("s3 hook init failed", "id", id, "error", err)
			// Non-fatal: sandbox is still usable
		}
	}

	e.logger.Info("sandbox created", "id", id, "image", cfg.Image)
	return sandbox, nil
}

// GetSandbox returns a sandbox by ID.
func (e *Engine) GetSandbox(id string) (*Sandbox, error) {
	v, ok := e.sandboxes.Load(id)
	if !ok {
		return nil, ErrNotFound
	}
	return v.(*Sandbox), nil
}

// ListSandboxes returns all tracked sandboxes.
func (e *Engine) ListSandboxes() []*Sandbox {
	var sandboxes []*Sandbox
	e.sandboxes.Range(func(_, v any) bool {
		sandboxes = append(sandboxes, v.(*Sandbox))
		return true
	})
	return sandboxes
}

// StopSandbox stops a sandbox without removing it.
func (e *Engine) StopSandbox(ctx context.Context, id string) error {
	sandbox, err := e.GetSandbox(id)
	if err != nil {
		return err
	}

	if err := e.runtime.Stop(ctx, id, 2*time.Second); err != nil {
		return fmt.Errorf("stopping sandbox: %w", err)
	}

	sandbox.SetStatus(runtime.StatusStopped)
	if err := e.saveSandbox(sandbox); err != nil {
		e.logger.Warn("failed to save sandbox state after stop", "id", id, "error", err)
	}
	e.logger.Info("sandbox stopped", "id", id)
	return nil
}

// StartSandbox starts a previously stopped sandbox without recreating it. The
// container (and its named volumes) survive Stop, so Start restores the same
// filesystem and identity.
func (e *Engine) StartSandbox(ctx context.Context, id string) error {
	sandbox, err := e.GetSandbox(id)
	if err != nil {
		return err
	}

	if err := e.runtime.Start(ctx, id); err != nil {
		return fmt.Errorf("starting sandbox: %w", err)
	}

	sandbox.SetStatus(runtime.StatusRunning)
	if err := e.saveSandbox(sandbox); err != nil {
		e.logger.Warn("failed to save sandbox state after start", "id", id, "error", err)
	}
	e.logger.Info("sandbox started", "id", id)
	return nil
}

// Info returns live runtime information for a sandbox, including the container
// IP. Works regardless of run state (does not require the sandbox to be
// running) so callers can resolve the address of a just-started sandbox.
func (e *Engine) Info(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	if _, err := e.GetSandbox(id); err != nil {
		return nil, err
	}
	return e.runtime.Info(ctx, id)
}

// DestroySandbox stops and removes a sandbox.
func (e *Engine) DestroySandbox(ctx context.Context, id string) error {
	// LoadAndDelete to prevent concurrent destroy of the same sandbox
	v, loaded := e.sandboxes.LoadAndDelete(id)
	if !loaded {
		return ErrNotFound
	}
	sb := v.(*Sandbox)

	// S3 hooks: upload files before stopping container (needs running container to read files)
	if sb.Config.Storage != nil && sb.Config.Storage.S3 != nil && sb.Config.Storage.S3.Mode == runtime.S3SyncModeHooks {
		if err := e.s3HookCleanup(ctx, id, sb.Config.Storage.S3); err != nil {
			e.logger.Warn("s3 hook cleanup failed", "id", id, "error", err)
		}
	}

	// Stop (ignore error if already stopped)
	_ = e.runtime.Stop(ctx, id, 1*time.Second)

	// Remove
	if err := e.runtime.Remove(ctx, id); err != nil {
		// Restore sandbox in map on failure so it can be retried
		e.sandboxes.Store(id, sb)
		return fmt.Errorf("removing sandbox: %w", err)
	}

	if err := e.store.DeleteSandbox(id); err != nil {
		e.logger.Warn("failed to delete sandbox from store", "id", id, "error", err)
	}
	e.decrementCount()

	e.logger.Info("sandbox destroyed", "id", id)
	return nil
}

// requireRunning returns nil if the sandbox exists and is running, or the
// reason it is not (not-found / not-running). Callers only need the gate, not
// the sandbox value.
func (e *Engine) requireRunning(id string) error {
	sb, err := e.GetSandbox(id)
	if err != nil {
		return err
	}
	if sb.GetStatus() != runtime.StatusRunning {
		return ErrNotRunning
	}
	return nil
}

// Exec runs a command in a sandbox.
func (e *Engine) Exec(ctx context.Context, id string, opts runtime.ExecOpts) (*runtime.ExecResult, error) {
	if err := e.requireRunning(id); err != nil {
		return nil, err
	}
	return e.runtime.Exec(ctx, id, opts)
}

// ExecStream runs a streaming command in a sandbox.
func (e *Engine) ExecStream(ctx context.Context, id string, opts runtime.ExecOpts) (runtime.ExecStream, error) {
	if err := e.requireRunning(id); err != nil {
		return nil, err
	}
	return e.runtime.ExecStream(ctx, id, opts)
}

// ReadFile reads a file from a sandbox.
func (e *Engine) ReadFile(ctx context.Context, id string, path string) ([]byte, error) {
	if err := e.requireRunning(id); err != nil {
		return nil, err
	}
	return e.runtime.ReadFile(ctx, id, path)
}

// WriteFile writes a file to a sandbox.
func (e *Engine) WriteFile(ctx context.Context, id string, path string, content []byte) error {
	if err := e.requireRunning(id); err != nil {
		return err
	}
	return e.runtime.WriteFile(ctx, id, path, content)
}

// ListDir lists a directory in a sandbox.
func (e *Engine) ListDir(ctx context.Context, id string, path string) ([]runtime.FileInfo, error) {
	if err := e.requireRunning(id); err != nil {
		return nil, err
	}
	return e.runtime.ListDir(ctx, id, path)
}

// Stat returns metadata for a single file or directory in a sandbox.
func (e *Engine) Stat(ctx context.Context, id string, path string) (*runtime.FileInfo, error) {
	if err := e.requireRunning(id); err != nil {
		return nil, err
	}
	return e.runtime.Stat(ctx, id, path)
}

// MkDir creates a directory in a sandbox.
func (e *Engine) MkDir(ctx context.Context, id string, path string) error {
	if err := e.requireRunning(id); err != nil {
		return err
	}
	return e.runtime.MkDir(ctx, id, path)
}

// RemoveFile removes a file from a sandbox.
func (e *Engine) RemoveFile(ctx context.Context, id string, path string) error {
	if err := e.requireRunning(id); err != nil {
		return err
	}
	return e.runtime.RemoveFile(ctx, id, path)
}

// Snapshot creates a snapshot of a sandbox.
func (e *Engine) Snapshot(ctx context.Context, id string, name string) (*runtime.SnapshotInfo, error) {
	if _, err := e.GetSandbox(id); err != nil {
		return nil, err
	}
	snap, err := e.runtime.Snapshot(ctx, id, name)
	if err != nil {
		return nil, err
	}
	// Save to store
	if err := e.store.SaveSnapshot(&store.SnapshotRecord{
		ID:        snap.ID,
		SandboxID: snap.SandboxID,
		Name:      snap.Name,
		ImageID:   snap.ImageID,
		CreatedAt: snap.CreatedAt,
	}); err != nil {
		e.logger.Warn("failed to save snapshot to store", "id", snap.ID, "error", err)
	}
	return snap, nil
}

// RestoreSnapshot restores a sandbox from a snapshot.
func (e *Engine) RestoreSnapshot(ctx context.Context, snapshotID string) (*Sandbox, error) {
	e.mu.Lock()
	// Check pressure under lock
	if pressure := e.pressureMonitor.CurrentEvent(); pressure.Level >= PressureCritical {
		e.mu.Unlock()
		return nil, ErrPressureTooHigh
	}
	if e.count >= e.config.MaxSandboxes {
		e.mu.Unlock()
		return nil, ErrLimitReached
	}
	e.count++
	e.mu.Unlock()

	snapRecord, err := e.store.GetSnapshot(snapshotID)
	if err != nil {
		// Try runtime directly
		newID, restoreErr := e.runtime.Restore(ctx, snapshotID)
		if restoreErr != nil {
			e.decrementCount()
			return nil, restoreErr
		}
		sandbox := &Sandbox{
			ID:        newID,
			status:    runtime.StatusRunning,
			CreatedAt: time.Now().UTC(),
		}
		e.sandboxes.Store(newID, sandbox)
		return sandbox, nil
	}

	newID, err := e.runtime.Restore(ctx, snapshotID)
	if err != nil {
		e.decrementCount()
		return nil, err
	}

	sandbox := &Sandbox{
		ID:        newID,
		Image:     snapRecord.ImageID,
		status:    runtime.StatusRunning,
		CreatedAt: time.Now().UTC(),
	}

	e.sandboxes.Store(newID, sandbox)
	if err := e.saveSandbox(sandbox); err != nil {
		e.logger.Warn("failed to save restored sandbox to store", "id", newID, "error", err)
	}

	e.logger.Info("sandbox restored from snapshot", "id", newID, "snapshot", snapshotID)
	return sandbox, nil
}

// ListSnapshots returns all snapshots for a sandbox.
func (e *Engine) ListSnapshots(ctx context.Context, sandboxID string) ([]runtime.SnapshotInfo, error) {
	return e.runtime.ListSnapshots(ctx, sandboxID)
}

// DeleteSnapshot removes a snapshot.
func (e *Engine) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	if err := e.runtime.RemoveSnapshot(ctx, snapshotID); err != nil {
		return err
	}
	if err := e.store.DeleteSnapshot(snapshotID); err != nil {
		e.logger.Warn("failed to delete snapshot from store", "id", snapshotID, "error", err)
	}
	return nil
}

// Stats returns stats for a sandbox.
func (e *Engine) Stats(ctx context.Context, id string) (*runtime.SandboxStats, error) {
	if _, err := e.GetSandbox(id); err != nil {
		return nil, err
	}
	return e.runtime.Stats(ctx, id)
}

// Shutdown gracefully stops all sandboxes.
// Uses a detached context so shutdown is not cancelled by the caller's context.
func (e *Engine) Shutdown(_ context.Context) {
	e.shutdownOnce.Do(func() {
		e.pressureMonitor.Stop() // Wait for monitor goroutine to finish before closing stopCh
		close(e.stopCh)

		// Use background context to ensure cleanup completes even if caller cancels
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Collect IDs first to avoid concurrent modification during Range
		var ids []string
		e.sandboxes.Range(func(key, _ any) bool {
			ids = append(ids, key.(string))
			return true
		})

		// Destroy sandboxes in parallel for faster shutdown
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				if err := e.DestroySandbox(shutdownCtx, id); err != nil {
					e.logger.Warn("failed to destroy sandbox on shutdown", "id", id, "error", err)
				}
			}(id)
		}
		wg.Wait()
	})
}

func (e *Engine) reaper() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.reapExpired()
		case <-e.stopCh:
			return
		}
	}
}

func (e *Engine) reapExpired() {
	ctx := context.Background()
	e.sandboxes.Range(func(_, value any) bool {
		sandbox := value.(*Sandbox)
		if sandbox.IsExpired() {
			e.logger.Info("reaping expired sandbox", "id", sandbox.ID)
			if err := e.DestroySandbox(ctx, sandbox.ID); err != nil {
				e.logger.Warn("failed to reap sandbox", "id", sandbox.ID, "error", err)
			}
		}
		return true
	})
}

func (e *Engine) restoreFromStore() {
	records, err := e.store.ListSandboxes()
	if err != nil {
		e.logger.Warn("failed to list sandboxes from store", "error", err)
		return
	}

	ctx := context.Background()
	restored := 0

	e.mu.Lock()
	for _, r := range records {
		// Verify container still exists in Docker
		info, err := e.runtime.Info(ctx, r.ID)
		if err != nil {
			e.logger.Warn("removing stale sandbox from store (container not found)", "id", r.ID)
			if delErr := e.store.DeleteSandbox(r.ID); delErr != nil {
				e.logger.Warn("failed to delete stale sandbox from store", "id", r.ID, "error", delErr)
			}
			continue
		}

		sandbox := &Sandbox{
			ID:        r.ID,
			Image:     r.Image,
			status:    info.Status,
			CreatedAt: r.CreatedAt,
			ExpiresAt: r.ExpiresAt,
		}
		e.sandboxes.Store(r.ID, sandbox)
		e.count++
		restored++
	}
	e.mu.Unlock()

	if restored > 0 {
		e.logger.Info("restored sandboxes from store", "count", restored)
	}
}

func (e *Engine) saveSandbox(sandbox *Sandbox) error {
	return e.store.SaveSandbox(&store.SandboxRecord{
		ID:        sandbox.ID,
		Image:     sandbox.Image,
		Status:    string(sandbox.GetStatus()),
		CreatedAt: sandbox.CreatedAt,
		ExpiresAt: sandbox.ExpiresAt,
	})
}

const (
	// maxS3SyncObjects limits the number of objects downloaded during S3 hook init.
	maxS3SyncObjects = 1000
	// maxS3ObjectSize limits the size of a single object during S3 hook init (100MB).
	maxS3ObjectSize = 100 * 1024 * 1024
	// s3HookTimeout is the timeout for S3 hook operations.
	s3HookTimeout = 5 * time.Minute
)

func (e *Engine) s3HookInit(ctx context.Context, id string, s3Cfg *runtime.S3SyncConfig) error {
	ctx, cancel := context.WithTimeout(ctx, s3HookTimeout)
	defer cancel()

	creds, err := storage.ResolveS3Credentials(s3Cfg, e.s3Config)
	if err != nil {
		return err
	}

	client, err := storage.NewS3Client(ctx, creds, e.logger)
	if err != nil {
		return err
	}

	syncPath := s3Cfg.SyncPath
	if syncPath == "" {
		syncPath = "/home/sandbox"
	}

	// List objects under the prefix and download each
	prefix := s3Cfg.Prefix
	keys, err := client.ListObjects(ctx, creds.Bucket, prefix, maxS3SyncObjects)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	var synced, skipped int
	for _, key := range keys {
		body, size, err := client.Download(ctx, creds.Bucket, key)
		if err != nil {
			e.logger.Warn("s3 hook: failed to download object", "key", key, "error", err)
			skipped++
			continue
		}

		// Skip objects that exceed size limit based on ContentLength header
		if size > maxS3ObjectSize {
			_ = body.Close()
			e.logger.Warn("s3 hook: object too large, skipping", "key", key, "size", size)
			skipped++
			continue
		}

		// Limit download size to prevent memory exhaustion
		buf.Reset()
		limited := io.LimitReader(body, maxS3ObjectSize+1)
		n, err := buf.ReadFrom(limited)
		_ = body.Close()
		if err != nil {
			skipped++
			continue
		}
		if n > maxS3ObjectSize {
			e.logger.Warn("s3 hook: object too large, skipping", "key", key, "size", n)
			skipped++
			continue
		}

		// Strip prefix from key for relative path and validate against traversal
		relPath := key
		if prefix != "" {
			relPath = strings.TrimPrefix(key, prefix)
			relPath = strings.TrimPrefix(relPath, "/")
		}
		destPath := filepath.Join(syncPath, filepath.Clean("/"+relPath))
		if !strings.HasPrefix(destPath, syncPath+"/") && destPath != syncPath {
			e.logger.Warn("s3 hook: path traversal detected, skipping", "key", key, "dest", destPath)
			skipped++
			continue
		}

		if err := e.runtime.WriteFile(ctx, id, destPath, buf.Bytes()); err != nil {
			e.logger.Warn("s3 hook: failed to write file", "path", destPath, "error", err)
			skipped++
			continue
		}
		synced++
	}

	e.logger.Info("s3 hook init completed", "id", id, "synced", synced, "skipped", skipped, "total", len(keys))
	return nil
}

func (e *Engine) s3HookCleanup(_ context.Context, id string, s3Cfg *runtime.S3SyncConfig) error {
	// Use background context: parent ctx may already be cancelled during destroy
	ctx, cancel := context.WithTimeout(context.Background(), s3HookTimeout)
	defer cancel()

	creds, err := storage.ResolveS3Credentials(s3Cfg, e.s3Config)
	if err != nil {
		return err
	}

	client, err := storage.NewS3Client(ctx, creds, e.logger)
	if err != nil {
		return err
	}

	syncPath := s3Cfg.SyncPath
	if syncPath == "" {
		syncPath = "/home/sandbox"
	}

	prefix := s3Cfg.Prefix

	// Recursively upload files from sync path
	return e.s3UploadDir(ctx, id, client, creds.Bucket, prefix, syncPath, syncPath, 0)
}

const maxUploadDepth = 20

// s3UploadDir recursively uploads all files in a directory to S3.
func (e *Engine) s3UploadDir(ctx context.Context, id string, client *storage.S3Client, bucket, prefix, basePath, dirPath string, depth int) error {
	if depth > maxUploadDepth {
		return fmt.Errorf("maximum directory depth %d exceeded at %s", maxUploadDepth, dirPath)
	}
	files, err := e.runtime.ListDir(ctx, id, dirPath)
	if err != nil {
		return fmt.Errorf("listing %s: %w", dirPath, err)
	}

	for _, f := range files {
		if f.IsDir {
			if err := e.s3UploadDir(ctx, id, client, bucket, prefix, basePath, f.Path, depth+1); err != nil {
				e.logger.Warn("s3 hook: failed to upload directory", "path", f.Path, "error", err)
			}
			continue
		}
		data, err := e.runtime.ReadFile(ctx, id, f.Path)
		if err != nil {
			e.logger.Warn("s3 hook: failed to read file for upload", "path", f.Path, "error", err)
			continue
		}
		if int64(len(data)) > maxS3ObjectSize {
			e.logger.Warn("s3 hook: file too large for upload, skipping", "path", f.Path, "size", len(data))
			continue
		}

		// Join the configured prefix and relative path with one separator;
		// a trailing slash in Prefix must not create an unreachable // key.
		relative := strings.TrimPrefix(strings.TrimPrefix(f.Path, basePath), "/")
		key := relative
		if prefix != "" {
			key = strings.TrimRight(prefix, "/") + "/" + relative
		}
		if err := client.Upload(ctx, bucket, key, bytes.NewReader(data), int64(len(data))); err != nil {
			e.logger.Warn("s3 hook: failed to upload file", "key", key, "error", err)
		}
	}
	return nil
}

func (e *Engine) validateStorage(sc *runtime.StorageConfig) error {
	if sc == nil {
		return nil
	}

	// Validate volumes
	if len(sc.Volumes) > 0 {
		if !e.config.AllowVolumes {
			return fmt.Errorf("volume mounts are not allowed by server policy")
		}
		if len(sc.Volumes) > e.config.MaxVolumesPerSandbox {
			return fmt.Errorf("too many volumes: %d exceeds limit of %d", len(sc.Volumes), e.config.MaxVolumesPerSandbox)
		}

		// Track volume names to detect shared volumes
		seen := make(map[string]bool)
		for _, vol := range sc.Volumes {
			if err := storage.ValidateVolumeName(vol.Name); err != nil {
				return fmt.Errorf("invalid volume name %q: %w", vol.Name, err)
			}
			if err := storage.ValidateVolumeMountPath(vol.MountPath); err != nil {
				return fmt.Errorf("invalid volume mount path %q: %w", vol.MountPath, err)
			}
			if seen[vol.Name] {
				return fmt.Errorf("duplicate volume name %q", vol.Name)
			}
			seen[vol.Name] = true
		}

		// Check shared volume policy — if a volume is used by other sandboxes
		if !e.config.AllowSharedVolumes {
			for _, vol := range sc.Volumes {
				if e.isVolumeInUse(vol.Name) {
					return fmt.Errorf("shared volumes are not allowed by server policy (volume %q is in use)", vol.Name)
				}
			}
		}
	}

	// Validate S3 config
	if sc.S3 != nil {
		if !e.config.AllowS3 {
			return fmt.Errorf("S3 sync is not allowed by server policy")
		}
		if sc.S3.Mode == runtime.S3SyncModeFUSE && !e.config.AllowS3FUSE {
			return fmt.Errorf("S3 FUSE mount is not allowed by server policy")
		}
		// Validate SyncPath
		if sc.S3.SyncPath != "" {
			if err := storage.ValidateVolumeMountPath(sc.S3.SyncPath); err != nil {
				return fmt.Errorf("invalid S3 sync_path %q: %w", sc.S3.SyncPath, err)
			}
		}
		// Validate MountPath
		if sc.S3.MountPath != "" {
			if err := storage.ValidateVolumeMountPath(sc.S3.MountPath); err != nil {
				return fmt.Errorf("invalid S3 mount_path %q: %w", sc.S3.MountPath, err)
			}
		}
	}

	return nil
}

// isVolumeInUse checks if a volume name is already mounted by another sandbox.
func (e *Engine) isVolumeInUse(volumeName string) bool {
	inUse := false
	e.sandboxes.Range(func(_, v any) bool {
		sb := v.(*Sandbox)
		if sb.Config.Storage != nil {
			for _, vol := range sb.Config.Storage.Volumes {
				if vol.Name == volumeName {
					inUse = true
					return false
				}
			}
		}
		return true
	})
	return inUse
}

func (e *Engine) decrementCount() {
	e.mu.Lock()
	e.count--
	if e.count < 0 {
		e.count = 0
	}
	e.mu.Unlock()
}
