package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/store"
)

// Expiry is measured from the last activity, not from creation. Before Touch
// existed the reaper destroyed any sandbox that outlived its timeout, however
// busy it was — a two-hour session on a thirty-minute timeout died four times.
func TestTouchExtendsFromNow(t *testing.T) {
	created := time.Now().UTC().Add(-25 * time.Minute)
	sb := &Sandbox{
		ID:        "s1",
		CreatedAt: created,
		Timeout:   30 * time.Minute,
		ExpiresAt: created.Add(30 * time.Minute),
	}

	// Five minutes from expiring.
	if sb.IsExpired() {
		t.Fatal("sandbox expired early")
	}

	before := sb.ExpiresAt
	sb.Touch()

	if !sb.ExpiresAt.After(before) {
		t.Fatalf("Touch did not move the deadline: %v -> %v", before, sb.ExpiresAt)
	}
	if remaining := time.Until(sb.ExpiresAt); remaining < 29*time.Minute {
		t.Fatalf("Touch gave only %v, expected close to the full 30m window", remaining)
	}
}

// An idle sandbox must still die. Touch that reset the clock unconditionally
// would leak every sandbox a user ever opened.
func TestUntouchedSandboxStillExpires(t *testing.T) {
	created := time.Now().UTC().Add(-2 * time.Hour)
	sb := &Sandbox{
		ID:        "s2",
		CreatedAt: created,
		Timeout:   30 * time.Minute,
		ExpiresAt: created.Add(30 * time.Minute),
	}
	if !sb.IsExpired() {
		t.Fatal("an idle sandbox past its window must be reapable")
	}
}

// "No timeout" means never, and Touch must not quietly invent one.
func TestTouchIsANoOpWithoutATimeout(t *testing.T) {
	sb := &Sandbox{ID: "s3", CreatedAt: time.Now().UTC()}
	sb.Touch()
	if !sb.ExpiresAt.IsZero() {
		t.Fatalf("Touch created an expiry out of nothing: %v", sb.ExpiresAt)
	}
	if sb.IsExpired() {
		t.Fatal("a sandbox with no timeout must never expire")
	}
}

// Timeout is persisted, because ExpiresAt is refreshed *from* it. A restart that
// dropped it would leave sandboxes holding a deadline they can no longer push
// back — the original bug, returning silently after the next restart.
func TestTimeoutSurvivesARestart(t *testing.T) {
	sb := &Sandbox{
		ID:        "s4",
		CreatedAt: time.Now().UTC(),
		Timeout:   0, // as if restored from a record written before Timeout existed
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	sb.Touch()
	if time.Until(sb.ExpiresAt) > 11*time.Minute {
		t.Fatal("a sandbox with no persisted timeout must not gain one from Touch")
	}

	restored := &Sandbox{
		ID:        "s5",
		CreatedAt: time.Now().UTC().Add(-20 * time.Minute),
		Timeout:   30 * time.Minute,
		ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
	}
	restored.Touch()
	if remaining := time.Until(restored.ExpiresAt); remaining < 29*time.Minute {
		t.Fatalf("restored sandbox got %v instead of its full window", remaining)
	}
}

// Touch and IsExpired both take the lock; the reaper reads while request
// handlers write. Run it under -race in CI.
func TestTouchIsConcurrencySafe(_ *testing.T) {
	sb := &Sandbox{
		ID:        "s6",
		CreatedAt: time.Now().UTC(),
		Timeout:   time.Minute,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			sb.Touch()
		}
		close(done)
	}()
	for i := 0; i < 500; i++ {
		_ = sb.IsExpired()
	}
	<-done
}

// Touches are not persisted, so a restored record carries a stale deadline. A
// restart must not reap a sandbox that was in use a moment before it.
func TestRestoreGivesAFreshIdleWindow(t *testing.T) {
	eng, _ := newTestEngine(t)
	require.NoError(t, eng.store.SaveSandbox(&store.SandboxRecord{
		ID:        "restored",
		Image:     "test:latest",
		CreatedAt: time.Now().UTC().Add(-time.Hour),
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
		Timeout:   10 * time.Minute,
	}))
	eng.restoreFromStore()

	sb, err := eng.GetSandbox("restored")
	require.NoError(t, err)
	assert.False(t, sb.IsExpired())
	assert.Greater(t, time.Until(sb.Expiry()), 9*time.Minute)
}

// A held operation (a proxied WebSocket, a blocking exec) keeps the sandbox
// alive past its deadline; releasing it counts as activity.
func TestHoldBlocksExpiry(t *testing.T) {
	sb := &Sandbox{ID: "h", Timeout: time.Minute, ExpiresAt: time.Now().UTC().Add(-time.Second)}
	release := sb.Hold()
	assert.False(t, sb.IsExpired(), "a held sandbox must not expire")
	release()
	release() // idempotent
	assert.False(t, sb.IsExpired(), "release must count as activity")
	sb.mu.Lock()
	sb.ExpiresAt = time.Now().UTC().Add(-time.Second)
	sb.mu.Unlock()
	assert.True(t, sb.IsExpired(), "once released, the deadline applies again")
}

// An exec stream holds its sandbox until it is closed, and only until then.
func TestHeldStreamReleasesOnClose(t *testing.T) {
	sb := &Sandbox{ID: "s", Timeout: time.Minute, ExpiresAt: time.Now().UTC().Add(-time.Second)}
	hs := &heldStream{ExecStream: nopStream{}, release: sb.Hold()}
	assert.False(t, sb.IsExpired(), "an open stream must hold the sandbox")
	require.NoError(t, hs.Close())
	require.NoError(t, hs.Close())
	sb.mu.RLock()
	held := sb.held
	sb.mu.RUnlock()
	assert.Equal(t, 0, held)
}

type nopStream struct{}

func (nopStream) Recv() (runtime.ExecStreamMessage, error) { return runtime.ExecStreamMessage{}, nil }
func (nopStream) Close() error                             { return nil }
