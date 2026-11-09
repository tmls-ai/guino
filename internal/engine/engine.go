package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
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
	// ErrHostBindRejected is a host bind the server refuses: disabled, or outside
	// every allowed root. The caller's to fix, so the API answers 422.
	ErrHostBindRejected = errors.New("host bind rejected")
	// ErrDraining refuses new placements on a host being emptied for removal.
	ErrDraining = errors.New("host is draining")
	// ErrArchiving is a sandbox whose state is being saved; it never runs again.
	// It wraps ErrNotRunning, so every "not running" answer (409) covers it.
	ErrArchiving = fmt.Errorf("%w: sandbox is archiving", ErrNotRunning)
	// ErrInUse refuses an unforced archive of a leased or held sandbox.
	ErrInUse = errors.New("sandbox is in use")
	// ErrBusy is a lifecycle call while another (stop, start, destroy) runs.
	ErrBusy = errors.New("sandbox is busy with another operation")
	// ErrNoState is an archive of a sandbox created without a state target.
	ErrNoState = errors.New("sandbox has no state target")
	// ErrInvalidState is a create request with an unusable state target (400).
	ErrInvalidState = errors.New("invalid state target")
	// ErrRestoreKeyNotFound is a create whose restore_key does not exist (422).
	ErrRestoreKeyNotFound = errors.New("restore_key not found")
)

// Engine orchestrates sandbox lifecycle.
type Engine struct {
	runtime        runtime.Runtime
	store          store.Store
	config         config.SandboxConfig
	s3Config       config.S3Config
	resourceConfig config.ResourceConfig
	netPolicy      netpolicy.Policy
	sandboxes      sync.Map // map[string]*Sandbox
	mu             sync.Mutex
	count          int
	// creating and draining are admission state, guarded by mu with count.
	creating int
	draining bool
	// reapSem bounds the reaper's workers; inflight lets tests wait for them.
	reapSem         chan struct{}
	inflight        sync.WaitGroup
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
	e.reapSem = make(chan struct{}, reapWorkers)
	e.sweepStateTemp()

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

	// One reaper pass at once, so an archive a restart cut off is finished
	// without waiting for the first tick. It only hands work to goroutines.
	e.reapExpired()
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
	if err := e.validateState(&cfg); err != nil {
		return nil, err
	}

	if err := e.admit(false, cfg.Storage); err != nil {
		return nil, err
	}
	defer e.doneCreating()

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
	// Host binds are validated before anything is created, so a refused bind is a
	// failed create rather than a running sandbox with a missing folder. The
	// resolved values replace the request in-place: the Docker boundary mounts
	// what it is handed, so nothing unvalidated may reach it.
	if len(cfg.HostBinds) > 0 {
		resolved, err := e.resolveHostBinds(cfg.HostBinds)
		if err != nil {
			e.decrementCount()
			return nil, err
		}
		cfg.HostBinds = resolved
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
	sandbox.lastActivity = sandbox.CreatedAt
	if timeout > 0 {
		sandbox.Timeout = timeout
		sandbox.ExpiresAt = sandbox.CreatedAt.Add(timeout)
	}

	// Create container
	if err := e.runtime.Create(ctx, id, cfg); err != nil {
		e.decrementCount()
		return nil, fmt.Errorf("creating sandbox: %w", err)
	}

	// Saved state goes in before anything runs. A missing key fails the create:
	// starting empty would look like a lost workspace.
	if cfg.State != nil && cfg.State.RestoreKey != "" {
		if err := e.restoreState(ctx, id, cfg.State); err != nil {
			e.discardContainer(ctx, sandbox)
			return nil, fmt.Errorf("restoring state from %s: %w", cfg.State.RestoreKey, err)
		}
	}

	// Start container
	if err := e.runtime.Start(ctx, id); err != nil {
		e.discardContainer(ctx, sandbox)
		return nil, fmt.Errorf("starting sandbox: %w", err)
	}

	sandbox.setStatus(runtime.StatusRunning)

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

// StopSandbox stops a sandbox without removing it. Refused (409) while an
// archive owns the sandbox; claimed for the call, so an archive cannot start
// in between and be undone by the status set afterwards.
func (e *Engine) StopSandbox(ctx context.Context, id string) error {
	sandbox, err := e.GetSandbox(id)
	if err != nil {
		return err
	}
	if err := sandbox.claim(); err != nil {
		return err
	}
	defer sandbox.release()

	if err := e.runtime.Stop(ctx, id, 2*time.Second); err != nil {
		return fmt.Errorf("stopping sandbox: %w", err)
	}

	sandbox.setStatus(runtime.StatusStopped)
	if err := e.saveSandbox(sandbox); err != nil {
		e.logger.Warn("failed to save sandbox state after stop", "id", id, "error", err)
	}
	e.logger.Info("sandbox stopped", "id", id)
	return nil
}

// StartSandbox starts a previously stopped sandbox without recreating it. The
// container (and its named volumes) survive Stop, so Start restores the same
// filesystem and identity. Like Stop, it holds the sandbox for the call and
// answers 409 while an archive owns it.
func (e *Engine) StartSandbox(ctx context.Context, id string) error {
	sandbox, err := e.GetSandbox(id)
	if err != nil {
		return err
	}
	if err := sandbox.claim(); err != nil {
		return err
	}
	defer sandbox.release()
	// A stopped container already holds its slot, so only draining and
	// pressure can refuse a start.
	if err := e.admit(true, nil); err != nil {
		return err
	}

	if err := e.runtime.Start(ctx, id); err != nil {
		return fmt.Errorf("starting sandbox: %w", err)
	}

	sandbox.setStatus(runtime.StatusRunning)
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
	sb, err := e.GetSandbox(id)
	if err != nil {
		return err
	}
	// One lifecycle operation at a time: an archive in progress answers 409.
	// An archiving sandbox nobody is working on (a failed or stuck archive)
	// may be destroyed: the caller accepts losing what was not saved.
	archiving, err := sb.claimDestroy()
	if err != nil {
		return err
	}
	if archiving {
		e.logger.Warn("destroying a sandbox whose archive did not finish, its unsaved state is lost", "id", id)
	}
	defer sb.release()
	e.sandboxes.Delete(id)

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

// resolveHostBinds validates each requested bind against the operator's roots.
func (e *Engine) resolveHostBinds(requested []runtime.HostBind) ([]runtime.HostBind, error) {
	if !e.netPolicy.HostBindsEnabled {
		return nil, fmt.Errorf("%w: host binds are disabled on this server (sandbox.allow_host_binds)", ErrHostBindRejected)
	}
	out := make([]runtime.HostBind, 0, len(requested))
	for _, b := range requested {
		if err := e.createMissingBindDir(b); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrHostBindRejected, err)
		}
		got, err := netpolicy.ValidateHostBind(
			netpolicy.HostBindRequest{
				HostPath:  b.HostPath,
				MountPath: b.MountPath,
				ReadOnly:  b.ReadOnly,
			},
			e.netPolicy.HostBindRoots,
			filepath.EvalSymlinks,
		)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrHostBindRejected, err)
		}
		// Directories only: a root that holds a socket (a user's
		// ~/.docker/run/docker.sock) must not hand that socket out on its own.
		if fi, err := os.Stat(got.HostPath); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("%w: host_path %q is not a directory", ErrHostBindRejected, b.HostPath)
		}
		out = append(out, runtime.HostBind{
			HostPath:  got.HostPath,
			MountPath: got.MountPath,
			ReadOnly:  got.ReadOnly,
		})
	}
	return out, nil
}

// requireRunning returns nil if the sandbox exists and is running, or the
// reason it is not (not-found / not-running / archiving). Callers only need
// the gate, not the sandbox value.
func (e *Engine) requireRunning(id string) error {
	release, err := e.acquire(id)
	if err != nil {
		return err
	}
	release()
	return nil
}

// acquire checks the sandbox is running and holds it in the same step, so an
// archive cannot begin between the check and the operation. Every operation
// *on* a running sandbox is activity, and they all pass through here: exec,
// streams, files, and sessions via Exec. Deliberately not in GetSandbox or
// Info: a dashboard polling status would otherwise keep an abandoned sandbox
// alive forever, which is the opposite of the point.
func (e *Engine) acquire(id string) (func(), error) {
	sb, err := e.GetSandbox(id)
	if err != nil {
		return nil, err
	}
	return sb.acquire()
}

// Exec runs a command in a sandbox.
func (e *Engine) Exec(ctx context.Context, id string, opts runtime.ExecOpts) (*runtime.ExecResult, error) {
	release, err := e.acquire(id)
	if err != nil {
		return nil, err
	}
	defer release()
	return e.runtime.Exec(ctx, id, opts)
}

// ExecStream runs a streaming command in a sandbox.
func (e *Engine) ExecStream(ctx context.Context, id string, opts runtime.ExecOpts) (runtime.ExecStream, error) {
	release, err := e.acquire(id)
	if err != nil {
		return nil, err
	}
	stream, err := e.runtime.ExecStream(ctx, id, opts)
	if err != nil {
		release()
		return nil, err
	}
	return &heldStream{ExecStream: stream, release: release}, nil
}

// heldStream keeps its sandbox from expiring until the stream is closed.
type heldStream struct {
	runtime.ExecStream
	release func()
}

func (s *heldStream) Close() error {
	s.release()
	return s.ExecStream.Close()
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
	if err := e.admit(false, nil); err != nil {
		return nil, err
	}
	defer e.doneCreating()

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

// Shutdown stops the engine. Sandboxes with a state target are left running
// and restored by the next start (restoreFromStore), so restarting Guino on a
// sandbox host never takes an agent's machine with it; they are saved one last
// time so the latest lease and activity survive. Sandboxes without state are
// destroyed as before: nothing would bring them back.
func (e *Engine) Shutdown(_ context.Context) {
	e.shutdownOnce.Do(func() {
		e.pressureMonitor.Stop() // Wait for monitor goroutine to finish before closing stopCh
		close(e.stopCh)

		// Use background context to ensure cleanup completes even if caller cancels
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		for _, sb := range e.ListSandboxes() {
			if sb.Config.State != nil {
				e.saveQuietly(sb)
				continue
			}
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				if err := e.DestroySandbox(shutdownCtx, id); err != nil {
					e.logger.Warn("failed to destroy sandbox on shutdown", "id", id, "error", err)
				}
			}(sb.ID)
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

// reapWorkers bounds the reaper's concurrent work (archives, removes,
// destroys). Each runs on its own goroutine with a timeout, so a stuck Docker
// or S3 call holds one worker, never the reaper.
const reapWorkers = 2

// reapTimeout bounds one destroy or remove started by the reaper.
const reapTimeout = time.Minute

// reapExpired hands every sandbox that has work due to a worker: an archive
// that did not finish, an idle sandbox with a state target (archived), and
// any other idle sandbox (destroyed). When both workers are busy the rest
// wait for the next tick.
func (e *Engine) reapExpired() {
	now := time.Now()
	e.sandboxes.Range(func(_, value any) bool {
		sb := value.(*Sandbox)
		if !sb.due(now) {
			return true
		}
		select {
		case e.reapSem <- struct{}{}:
		default:
			return false
		}
		e.inflight.Add(1)
		go func() {
			defer e.inflight.Done()
			defer func() { <-e.reapSem }()
			e.reapOne(sb, now)
		}()
		return true
	})
}

func (e *Engine) reapOne(sb *Sandbox, now time.Time) {
	switch {
	case sb.claimResume(now):
		if err := e.resumeArchive(sb); err != nil {
			e.logger.Warn("failed to finish archive", "id", sb.ID, "error", err)
		}
	case !sb.IsExpired():
	case sb.Config.State != nil && !sb.isNeverStarted():
		e.logger.Info("archiving idle sandbox", "id", sb.ID)
		err := e.ArchiveSandbox(context.Background(), sb.ID, false)
		if err != nil && !errors.Is(err, ErrInUse) && !errors.Is(err, ErrArchiving) && !errors.Is(err, ErrBusy) {
			e.logger.Warn("failed to archive idle sandbox", "id", sb.ID, "error", err)
		}
	default:
		e.logger.Info("reaping expired sandbox", "id", sb.ID)
		ctx, cancel := context.WithTimeout(context.Background(), reapTimeout)
		defer cancel()
		if err := e.DestroySandbox(ctx, sb.ID); err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrNotFound) {
			sb.failed()
			e.logger.Warn("failed to reap sandbox", "id", sb.ID, "error", err)
		}
	}
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

		var cfg runtime.SandboxConfig
		if len(r.Config) > 0 {
			if err := json.Unmarshal(r.Config, &cfg); err != nil {
				e.logger.Warn("unreadable sandbox config in store, restoring without it", "id", r.ID, "error", err)
			}
		}
		if cfg.Labels == nil {
			cfg.Labels = r.Labels
		}
		sandbox := &Sandbox{
			ID:              r.ID,
			Image:           r.Image,
			status:          info.Status,
			Config:          cfg,
			Ports:           cfg.Ports,
			CreatedAt:       r.CreatedAt,
			ExpiresAt:       r.ExpiresAt,
			Timeout:         r.Timeout,
			leasedUntil:     r.LeasedUntil,
			lastLeasedAt:    r.LastLeasedAt,
			archiveUploaded: r.ArchiveUploaded,
			neverStarted:    r.NeverStarted,
			stuck:           r.Stuck,
		}
		// An archive had begun before the restart (maybe its upload even
		// finished): the sandbox must never run again, whatever Docker says. The
		// reaper finishes it: a fresh upload (or a true 412) and the remove.
		if r.ArchiveUploaded || r.Status == string(runtime.StatusArchiving) {
			sandbox.status = runtime.StatusArchiving
		} else if info.Paused {
			// Frozen by an archive whose unpause failed before the restart, and
			// not archiving: give it back.
			uctx, cancel := context.WithTimeout(ctx, reapTimeout)
			if err := e.runtime.Unpause(uctx, r.ID); err != nil {
				e.logger.Warn("failed to unpause a restored sandbox", "id", r.ID, "error", err)
			} else {
				sandbox.status = runtime.StatusRunning
			}
			cancel()
		}
		// Records written before Timeout was persisted held a fixed deadline of
		// created+timeout, which recovers the window exactly.
		if sandbox.Timeout == 0 && !r.ExpiresAt.IsZero() {
			sandbox.Timeout = r.ExpiresAt.Sub(r.CreatedAt)
		}
		// Touches are not persisted, so the stored deadline is stale. A restart
		// is not idleness: give each sandbox a fresh window rather than reaping
		// one that was in use a moment ago. No-op for records without a Timeout.
		// The recorded last activity is kept for the controller.
		sandbox.Touch()
		if !r.LastActivity.IsZero() {
			sandbox.lastActivity = r.LastActivity
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
	return e.store.SaveSandbox(sandbox.record())
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

// createMissingBindDir creates a host bind's source directory when it does not
// exist yet (a project workspace on a shared mount, first placement), and only
// inside a writable allowed root: the directories are made through os.Root,
// which refuses any path that would leave the root, symlinks included. The
// result then goes through the usual validation like any existing path.
func (e *Engine) createMissingBindDir(b runtime.HostBind) error {
	p := b.HostPath
	if _, err := os.Lstat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return fmt.Errorf("host_path %q must be absolute and clean to be created", p)
	}
	for _, root := range e.netPolicy.HostBindRoots {
		if root.ReadOnly {
			continue
		}
		// The request may name the root as configured or as resolved.
		bases := []string{root.Path}
		if resolved, err := filepath.EvalSymlinks(root.Path); err == nil && resolved != root.Path {
			bases = append(bases, resolved)
		}
		for _, base := range bases {
			rel, err := filepath.Rel(base, p)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			return mkdirInRoot(base, rel)
		}
	}
	return fmt.Errorf("host_path %q does not exist and is not inside a writable allowed root", p)
}

func mkdirInRoot(base, rel string) error {
	r, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("opening host bind root %q: %w", base, err)
	}
	defer func() { _ = r.Close() }()
	if err := r.MkdirAll(rel, 0o755); err != nil {
		return fmt.Errorf("creating %q under %q: %w", rel, base, err)
	}
	return nil
}
