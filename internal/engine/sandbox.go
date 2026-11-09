package engine

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/store"
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
	// leasedUntil is the end of the caller's lease (see Lease); lastLeasedAt
	// and lastActivity are reported to the controller. Guarded by mu.
	leasedUntil  time.Time
	lastLeasedAt time.Time
	lastActivity time.Time
	// archiveUploaded means the state is safely at its archive key and only
	// the container remove is left; the sandbox never runs again. busy marks
	// an archive, stop, start or destroy in flight, so only one runs at a time. Guarded by mu.
	archiveUploaded bool
	busy            bool
	// neverStarted is a container whose create failed after it existed: it
	// has no state worth saving. failures and nextAttempt back the reaper
	// off after a failed attempt. Guarded by mu.
	neverStarted bool
	// stuck is an archive that cannot succeed (state over max_bytes): the
	// reaper leaves it; DELETE or a forced archive settles it.
	stuck       bool
	failures    int
	nextAttempt time.Time
}

// Touch records activity and pushes the expiry out by the sandbox's idle window.
//
// Expiry is measured from the last activity, not from creation. An absolute
// deadline destroys the sandbox out from under a user who is still working,
// which is exactly what the reaper did before this existed, on any run longer
// than the timeout. Daytona's auto_stop_interval has the same idle semantics,
// and the backend's behaviour is written against it.
//
// The expiry is unchanged when the sandbox has no timeout, so "never expires"
// stays never.
func (s *Sandbox) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touchLocked(time.Now().UTC())
}

func (s *Sandbox) touchLocked(now time.Time) {
	s.lastActivity = now
	if s.Timeout <= 0 || s.ExpiresAt.IsZero() {
		return
	}
	s.ExpiresAt = now.Add(s.Timeout)
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
	return s.releaser()
}

func (s *Sandbox) releaser() func() {
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

// acquire is Hold for an operation that needs a running sandbox: the status
// check and the hold are one step, so an archive cannot start in between.
func (s *Sandbox) acquire() (release func(), err error) {
	s.mu.Lock()
	if err := s.runningLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.held++
	s.touchLocked(time.Now().UTC())
	s.mu.Unlock()
	return s.releaser(), nil
}

func (s *Sandbox) runningLocked() error {
	switch s.status {
	case runtime.StatusRunning:
		return nil
	case runtime.StatusArchiving:
		return ErrArchiving
	default:
		return ErrNotRunning
	}
}

// Lease keeps the sandbox from being archived or reaped until now+ttl,
// whatever its traffic, and counts as activity. Refused while archiving.
func (s *Sandbox) Lease(ttl time.Duration) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == runtime.StatusArchiving {
		return time.Time{}, ErrArchiving
	}
	now := time.Now().UTC()
	s.leasedUntil = now.Add(ttl)
	s.lastLeasedAt = now
	s.touchLocked(now)
	return s.leasedUntil, nil
}

// beginArchive checks for a live lease and held operations (unless forced)
// and moves to archiving in one step under the lock, which is what makes a
// lease and the reaper unable to race. It returns the status to go back to if
// the upload fails.
func (s *Sandbox) beginArchive(force bool) (runtime.SandboxStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.claimableLocked(); err != nil {
		return "", err
	}
	if !force && (s.held > 0 || s.leasedUntil.After(time.Now())) {
		return "", ErrInUse
	}
	prev := s.status
	s.status = runtime.StatusArchiving
	s.busy = true
	return prev, nil
}

// abortArchive is a failed upload in this process: the sandbox is usable again.
func (s *Sandbox) abortArchive(prev runtime.SandboxStatus) {
	s.mu.Lock()
	s.status = prev
	s.busy = false
	s.mu.Unlock()
}

func (s *Sandbox) markUploaded() {
	s.mu.Lock()
	s.archiveUploaded = true
	s.mu.Unlock()
}

func (s *Sandbox) uploaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.archiveUploaded
}

// claim takes the sandbox for one lifecycle operation (stop, start, destroy).
// Busy keeps it from overlapping an archive, so a status set after a Docker
// call can never resurrect an archived sandbox.
func (s *Sandbox) claim() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.claimableLocked(); err != nil {
		return err
	}
	s.busy = true
	return nil
}

func (s *Sandbox) claimableLocked() error {
	if s.status == runtime.StatusArchiving || s.archiveUploaded {
		return ErrArchiving
	}
	if s.busy {
		return ErrBusy
	}
	return nil
}

// claimResume takes an archive that did not finish (an upload or remove that
// failed, or one a restart cut off) for one more attempt, once its backoff
// has passed.
func (s *Sandbox) claimResume(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy || s.stuck || s.status != runtime.StatusArchiving || now.Before(s.nextAttempt) {
		return false
	}
	s.busy = true
	return true
}

func (s *Sandbox) release() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

// retryBackoff spaces the reaper's attempts on one sandbox after failures.
var retryBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute}

// failed records a failed reaper operation and when to try again.
func (s *Sandbox) failed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.nextAttempt = time.Now().Add(retryBackoff[min(s.failures, len(retryBackoff))-1])
}

// due reports whether the reaper has anything to do with the sandbox now: an
// unfinished archive, or idleness, and no backoff pending.
func (s *Sandbox) due(now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.busy || s.stuck || now.Before(s.nextAttempt) {
		return false
	}
	return s.status == runtime.StatusArchiving || s.expiredLocked(now)
}

// IsExpired returns true if the sandbox has been idle past its timeout. A
// held operation, a live lease or an archive in progress is never idle.
func (s *Sandbox) IsExpired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.expiredLocked(time.Now())
}

func (s *Sandbox) expiredLocked(now time.Time) bool {
	if s.ExpiresAt.IsZero() || s.held > 0 || s.status == runtime.StatusArchiving || s.leasedUntil.After(now) {
		return false
	}
	return now.After(s.ExpiresAt)
}

// setStatus moves to status unless the sandbox is archiving: once an archive
// began, nothing but the archive path changes the status.
func (s *Sandbox) setStatus(status runtime.SandboxStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != runtime.StatusArchiving {
		s.status = status
	}
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

// Activity is the lease and activity view the controller decides on.
type Activity struct {
	LeasedUntil  time.Time `json:"leased_until"`
	LastLeasedAt time.Time `json:"last_leased_at"`
	LastActivity time.Time `json:"last_activity"`
}

// Activity returns the sandbox's lease and activity times (thread-safe).
func (s *Sandbox) Activity() Activity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Activity{LeasedUntil: s.leasedUntil, LastLeasedAt: s.lastLeasedAt, LastActivity: s.lastActivity}
}

// record is what a restart needs to keep the sandbox: its config (without
// env and S3 keys, which only matter at create and are secrets), labels, ports,
// lease and state target, and whether an archive upload already finished.
func (s *Sandbox) record() *store.SandboxRecord {
	cfg := s.Config
	cfg.Env = nil
	// Per-sandbox S3 keys stay in memory only. Copies, so the live config
	// keeps them for the hook cleanup.
	if cfg.Storage != nil {
		st := *cfg.Storage
		if st.S3 != nil {
			s3 := *st.S3
			s3.AccessKey, s3.SecretKey = "", ""
			st.S3 = &s3
		}
		cfg.Storage = &st
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		raw = nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &store.SandboxRecord{
		ID:              s.ID,
		Image:           s.Image,
		Status:          string(s.status),
		Config:          raw,
		Labels:          cfg.Labels,
		CreatedAt:       s.CreatedAt,
		ExpiresAt:       s.ExpiresAt,
		Timeout:         s.Timeout,
		LeasedUntil:     s.leasedUntil,
		LastLeasedAt:    s.lastLeasedAt,
		LastActivity:    s.lastActivity,
		ArchiveUploaded: s.archiveUploaded,
		NeverStarted:    s.neverStarted,
		Stuck:           s.stuck,
	}
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

func (s *Sandbox) markNeverStarted() {
	s.mu.Lock()
	s.status = runtime.StatusError
	s.neverStarted = true
	s.mu.Unlock()
}

func (s *Sandbox) isNeverStarted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.neverStarted
}

// claimDestroy takes the sandbox for an explicit destroy. Unlike claim it
// accepts an archiving sandbox nobody is working on: a DELETE accepts losing
// state that was not saved, which is what lets a host with a failed archive
// drain to zero. It reports whether unsaved state is being given up.
func (s *Sandbox) claimDestroy() (archiving bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		if s.status == runtime.StatusArchiving {
			return false, ErrArchiving
		}
		return false, ErrBusy
	}
	s.busy = true
	return s.status == runtime.StatusArchiving && !s.archiveUploaded, nil
}

// markStuck ends an archive attempt that cannot succeed: archiving, not busy,
// and left alone by the reaper.
func (s *Sandbox) markStuck() {
	s.mu.Lock()
	s.status = runtime.StatusArchiving
	s.stuck = true
	s.busy = false
	s.mu.Unlock()
}

// claimStuck takes a stuck archive for a forced retry.
func (s *Sandbox) claimStuck() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stuck || s.busy {
		return false
	}
	s.stuck = false
	s.busy = true
	return true
}
