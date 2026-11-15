package engine

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/engine/enginetest"
	"github.com/tmls-ai/guino/internal/runtime"
)

const gib = int64(1024 * 1024 * 1024)

func limitsFor(calls []enginetest.MemoryLimitCall, id string) []int64 {
	var out []int64
	for _, c := range calls {
		if c.ID == id {
			out = append(out, c.Bytes)
		}
	}
	return out
}

func TestMemoryHighTarget(t *testing.T) {
	const floor = 32 << 20
	tests := []struct {
		name                     string
		shared, floor, requested int64
		want                     int64
	}{
		{"share below request is kept", 1 * gib, floor, 5 * gib, 1 * gib},
		{"share above request is clamped to request", 8 * gib, floor, 5 * gib, 5 * gib},
		{"no request means no cap", 8 * gib, floor, 0, 8 * gib},
		{"floor applies below share", 0, floor, 5 * gib, floor},
		{"floor never exceeds request", 0, floor, 16 << 20, 16 << 20},
		{"never 0, which would remove the limit", 0, 0, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, memoryHighTarget(tt.shared, tt.floor, tt.requested))
		})
	}
}

// The reproduction: an 8 GiB Mac under pressure, one sandbox that asked for
// 5 GiB. The old code applied 80% of free host memory (about 1.89 GiB) to it.
// A soft limit may go below the request; it must never go above it.
func TestPressureThrottle_SandboxWithRequestKeepsItsCap(t *testing.T) {
	eng, mock := newTestEngine(t)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Memory: 5 * gib})
	require.NoError(t, err)

	// Plenty free: the share (80% of 7 GiB) is above the request.
	eng.onPressureChange(PressureEvent{Level: PressureHigh, MemoryTotal: 8 * uint64(gib), MemoryUsed: 1 * uint64(gib)})
	// Little free: the share is below the request.
	eng.onPressureChange(PressureEvent{Level: PressureCritical, MemoryTotal: 8 * uint64(gib), MemoryUsed: 7 * uint64(gib)})
	// Pressure eased: the soft limit is removed, nothing else.
	eng.onPressureChange(PressureEvent{Level: PressureNormal, MemoryTotal: 8 * uint64(gib)})

	got := limitsFor(mock.MemoryLimits(), sb.ID)
	require.Len(t, got, 3)
	assert.Equal(t, 5*gib, got[0], "soft limit above the request")
	free := float64(gib)
	assert.Equal(t, int64(free*memoryHighSafetyFactor), got[1], "soft limit below the request")
	assert.Equal(t, int64(0), got[2], "0 removes memory.high only")
}

// Where the runtime reports no soft limit (macOS/Docker Desktop), the engine
// stops after the first refusal, logs once and never calls the runtime again.
func TestPressureThrottle_UnsupportedRuntimeStopsThrottling(t *testing.T) {
	eng, mock := newTestEngine(t)
	mock.UpdateMemoryLimitFn = func(string, int64) error {
		return fmt.Errorf("%w: test", runtime.ErrMemoryThrottleUnsupported)
	}
	for range 2 {
		_, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Memory: 5 * gib})
		require.NoError(t, err)
	}

	evt := PressureEvent{Level: PressureCritical, MemoryTotal: 8 * uint64(gib), MemoryUsed: 7 * uint64(gib)}
	eng.onPressureChange(evt)
	eng.onPressureChange(evt)
	eng.onPressureChange(PressureEvent{Level: PressureNormal, MemoryTotal: 8 * uint64(gib)})

	assert.Len(t, mock.MemoryLimits(), 1, "first refusal ends throttling on this host")
	assert.True(t, eng.throttleUnsupported.Load())
}

// Once a sandbox is throttled, a later "unsupported" must not strand it:
// release still resets memory.high on every sandbox that has one.
func TestPressureThrottle_ReleaseAfterUnsupportedReachesThrottledSandboxes(t *testing.T) {
	eng, mock := newTestEngine(t)
	var ids []string
	for range 2 {
		sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Memory: 5 * gib})
		require.NoError(t, err)
		ids = append(ids, sb.ID)
	}
	critical := PressureEvent{Level: PressureCritical, MemoryTotal: 8 * uint64(gib), MemoryUsed: 7 * uint64(gib)}

	eng.onPressureChange(critical) // both throttled
	mock.UpdateMemoryLimitFn = func(_ string, v int64) error {
		if v > 0 {
			return fmt.Errorf("%w: test", runtime.ErrMemoryThrottleUnsupported)
		}
		return nil
	}
	eng.onPressureChange(critical) // refused: throttling switches off
	require.True(t, eng.throttleUnsupported.Load())
	eng.onPressureChange(PressureEvent{Level: PressureNormal, MemoryTotal: 8 * uint64(gib)})

	for _, id := range ids {
		got := limitsFor(mock.MemoryLimits(), id)
		require.NotEmpty(t, got)
		assert.Equal(t, int64(0), got[len(got)-1], "sandbox %s released", id)
	}
}
