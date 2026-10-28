//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
)

// busybox has no tmux. Every session call must say so (501 at the API), which
// depends on a real daemon reporting exit 127 for a missing binary.
func TestIntegration_Sessions_ImageWithoutTmuxIsUnsupported(t *testing.T) {
	eng := newNetEngine(t, runtime.NetworkModeNone)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Image: netTestImage})
	require.NoError(t, err)
	ctx := context.Background()

	err = eng.CreateSession(ctx, sb.ID, "web")
	assert.True(t, errors.Is(err, engine.ErrSessionUnsupported), "create: %v", err)
	_, err = eng.ListSessions(ctx, sb.ID)
	assert.True(t, errors.Is(err, engine.ErrSessionUnsupported), "list: %v", err)
	err = eng.SessionExec(ctx, sb.ID, "web", "ls")
	assert.True(t, errors.Is(err, engine.ErrSessionUnsupported), "exec: %v", err)
}
