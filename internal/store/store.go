package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
)

// SandboxRecord represents a persisted sandbox entry.
type SandboxRecord struct {
	ID     string `json:"id"`
	Image  string `json:"image"`
	Status string `json:"status"`
	// Config is the sandbox's runtime.SandboxConfig as JSON (the store stays
	// an import leaf), so a restart keeps ports, storage and the state target.
	Config    json.RawMessage   `json:"config,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	ExpiresAt time.Time         `json:"expires_at,omitempty"`
	// Timeout is the idle window, persisted because ExpiresAt is refreshed from
	// it on every operation. Without it a Guino restart leaves sandboxes with a
	// deadline they can no longer push back, so the next one silently reverts to
	// destroying a sandbox someone is still using.
	Timeout time.Duration `json:"timeout,omitempty"`
	// Lease and activity, so a restart does not forget a run that holds the
	// sandbox or make an idle one look fresh to the controller.
	LeasedUntil  time.Time `json:"leased_until,omitempty"`
	LastLeasedAt time.Time `json:"last_leased_at,omitempty"`
	LastActivity time.Time `json:"last_activity,omitempty"`
	// ArchiveUploaded is an archive whose upload finished: a restart resumes
	// the container remove instead of running the sandbox again.
	ArchiveUploaded bool `json:"archive_uploaded,omitempty"`
	// NeverStarted is a container whose create failed: not worth archiving.
	NeverStarted bool `json:"never_started,omitempty"`
	// Stuck is an archive that cannot succeed (state over max_bytes): a restart
	// does not retry it; DELETE or a forced archive settles it.
	Stuck bool `json:"stuck,omitempty"`
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
