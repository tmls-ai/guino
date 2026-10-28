package store

import (
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
)

// SandboxRecord represents a persisted sandbox entry.
type SandboxRecord struct {
	ID        string            `json:"id"`
	Image     string            `json:"image"`
	Status    string            `json:"status"`
	Config    map[string]any    `json:"config,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at,omitempty"`
	// Timeout is the idle window, persisted because ExpiresAt is refreshed from
	// it on every operation. Without it a Guino restart leaves sandboxes with a
	// deadline they can no longer push back, so the next one silently reverts to
	// destroying a sandbox someone is still using.
	Timeout time.Duration `json:"timeout,omitempty"`
}

// SnapshotRecord represents a persisted snapshot entry.
type SnapshotRecord struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	Name      string    `json:"name"`
	ImageID   string    `json:"image_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Store defines the interface for state persistence.
type Store interface {
	// Sandbox operations
	SaveSandbox(record *SandboxRecord) error
	GetSandbox(id string) (*SandboxRecord, error)
	ListSandboxes() ([]*SandboxRecord, error)
	DeleteSandbox(id string) error

	// Snapshot operations
	SaveSnapshot(record *SnapshotRecord) error
	GetSnapshot(id string) (*SnapshotRecord, error)
	ListSnapshots(sandboxID string) ([]*SnapshotRecord, error)
	DeleteSnapshot(id string) error

	// Close the store
	Close() error
}
