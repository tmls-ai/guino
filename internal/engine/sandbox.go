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
	// Timeout is the idle window ExpiresAt is refreshed by. Stored because the
	// per-sandbox value can differ from the server default, and a refresh that
	// used the default would silently change a caller's chosen timeout.
	Timeout time.Duration         `json:"timeout,omitempty"`
	Ports   []runtime.PortMapping `json:"ports,omitempty"`
	mu      sync.RWMutex
	// held counts in-flight long operations (see Hold), guarded by mu.
	held int
}

// Touch pushes the expiry out by the sandbox's idle window.
//
// Expiry is measured from the last activity, not from creation. An absolute
// deadline destroys the sandbox out from under a user who is still working,
// which is exactly what the reaper did before this existed, on any run longer
// than the timeout. Daytona's auto_stop_interval has the same idle semantics,
// and the backend's behaviour is written against it.
//
// No-op when the sandbox has no timeout, so "never expires" stays never.
func (s *Sandbox) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Timeout <= 0 || s.ExpiresAt.IsZero() {
		return
	}
	s.ExpiresAt = time.Now().UTC().Add(s.Timeout)
}

// Expiry returns the current idle deadline (thread-safe: Touch moves it).
func (s *Sandbox) Expiry() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ExpiresAt
}

// Hold marks a long operation in progress (a proxied WebSocket, a blocking
// exec): the sandbox does not expire while any is held, however long it runs.
// The returned release is idempotent and counts as activity.
func (s *Sandbox) Hold() (release func()) {
	s.mu.Lock()
	s.held++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			s.held--
			s.mu.Unlock()
			s.Touch()
		})
	}
}

// IsExpired returns true if the sandbox has been idle past its timeout.
func (s *Sandbox) IsExpired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ExpiresAt.IsZero() || s.held > 0 {
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
		ExpiresAt: s.Expiry(),
		Ports:     s.Ports,
	})
}
