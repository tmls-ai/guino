package docker

import (
	"errors"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/stretchr/testify/assert"

	"github.com/tmls-ai/guino/internal/runtime"
)

// Docker refuses an exec in a container that is not running with a conflict;
// only that is marked, so the engine never restarts a container for an
// unrelated exec failure.
func TestExecCreateError(t *testing.T) {
	refused := fmt.Errorf("container abc is not running: %w", cerrdefs.ErrConflict)
	assert.ErrorIs(t, execCreateError(refused), runtime.ErrContainerNotRunning)
	assert.ErrorIs(t, execCreateError(refused), cerrdefs.ErrConflict)
	assert.NotErrorIs(t, execCreateError(errors.New("connection refused")), runtime.ErrContainerNotRunning)
}
