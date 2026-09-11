package engine

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/tmls-ai/guino/internal/runtime"
)

// Sandbox represents a managed sandbox instance.
type Sandbox struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	status    runtime.SandboxStatus
	Config    runtime.SandboxConfig `json:"-"`
	CreatedAt time.Time             `json:"created_at"`
	ExpiresAt time.Time             `json:"expires_at,omitempty"`
	Ports     []runtime.PortMapping `json:"ports,omitempty"`
	mu        sync.RWMutex
}

// IsExpired returns true if the sandbox has exceeded its timeout.
func (s *Sandbox) IsExpired() bool {
	if s.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(s.ExpiresAt)
}

// SetStatus updates the sandbox status (thread-safe).
func (s *Sandbox) SetStatus(status runtime.SandboxStatus) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// GetStatus returns the sandbox status (thread-safe).
func (s *Sandbox) GetStatus() runtime.SandboxStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// MarshalJSON implements custom JSON marshaling to include the status field.
func (s *Sandbox) MarshalJSON() ([]byte, error) {
	type Alias struct {
		ID        string                `json:"id"`
		Image     string                `json:"image"`
		Status    runtime.SandboxStatus `json:"status"`
		CreatedAt time.Time             `json:"created_at"`
		ExpiresAt time.Time             `json:"expires_at,omitempty"`
		Ports     []runtime.PortMapping `json:"ports,omitempty"`
	}
	return json.Marshal(&Alias{
		ID:        s.ID,
		Image:     s.Image,
		Status:    s.GetStatus(),
		CreatedAt: s.CreatedAt,
		ExpiresAt: s.ExpiresAt,
		Ports:     s.Ports,
	})
}
