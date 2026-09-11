package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/tmls-ai/guino/internal/runtime"
)

// WarmPool pre-creates containers so sandbox creation is near-instant.
type WarmPool struct {
	runtime runtime.Runtime
	image   string
	config  runtime.SandboxConfig
	pool    chan string // buffered channel of ready container IDs
	size    int
	mu      sync.Mutex
	logger  *slog.Logger
	stopCh  chan struct{}
	stopped bool
}

// NewWarmPool creates a warm pool that maintains `size` pre-created containers.
func NewWarmPool(rt runtime.Runtime, size int, cfg runtime.SandboxConfig, logger *slog.Logger) *WarmPool {
	if size <= 0 {
		return nil
	}

	wp := &WarmPool{
		runtime: rt,
		image:   cfg.Image,
		config:  cfg,
		pool:    make(chan string, size),
		size:    size,
		logger:  logger,
		stopCh:  make(chan struct{}),
	}

	// Fill the pool initially
	go wp.fill()

	return wp
}

// Acquire gets a pre-warmed container ID from the pool.
// Returns empty string if pool is empty (caller should create normally).
func (wp *WarmPool) Acquire() string {
	if wp == nil {
		return ""
	}
	select {
	case id := <-wp.pool:
		// Replenish in background
		go wp.addOne()
		return id
	default:
		return ""
	}
}

// Return puts an unused container ID back to the pool or removes it.
func (wp *WarmPool) Return(ctx context.Context, id string) {
	if wp == nil {
		return
	}
	// Don't return, just remove it (it may have been modified)
	_ = wp.runtime.Stop(ctx, id, 1*time.Second)
	_ = wp.runtime.Remove(ctx, id)
}

// Size returns the current number of warm containers.
func (wp *WarmPool) Size() int {
	if wp == nil {
		return 0
	}
	return len(wp.pool)
}

// Stop drains and destroys all warm containers.
func (wp *WarmPool) Stop(ctx context.Context) {
	if wp == nil {
		return
	}
	wp.mu.Lock()
	wp.stopped = true
	close(wp.stopCh)
	wp.mu.Unlock()

	// Drain the pool
	for {
		select {
		case id := <-wp.pool:
			_ = wp.runtime.Stop(ctx, id, 1*time.Second)
			_ = wp.runtime.Remove(ctx, id)
		default:
			return
		}
	}
}

func (wp *WarmPool) fill() {
	for i := 0; i < wp.size; i++ {
		select {
		case <-wp.stopCh:
			return
		default:
			wp.addOne()
		}
	}
	wp.logger.Info("warm pool filled", "size", wp.Size(), "target", wp.size)
}

func (wp *WarmPool) addOne() {
	wp.mu.Lock()
	if wp.stopped {
		wp.mu.Unlock()
		return
	}
	wp.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use a temporary ID for the warm container
	id := "warm-" + time.Now().Format("20060102150405.000000000")

	cfg := wp.config
	if cfg.Image == "" {
		cfg.Image = wp.image
	}

	if err := wp.runtime.Create(ctx, id, cfg); err != nil {
		wp.logger.Warn("warm pool: failed to create container", "error", err)
		return
	}

	if err := wp.runtime.Start(ctx, id); err != nil {
		_ = wp.runtime.Remove(ctx, id)
		wp.logger.Warn("warm pool: failed to start container", "error", err)
		return
	}

	select {
	case wp.pool <- id:
		// Added to pool
	default:
		// Pool is full, remove the extra container
		_ = wp.runtime.Stop(ctx, id, 1*time.Second)
		_ = wp.runtime.Remove(ctx, id)
	}
}
