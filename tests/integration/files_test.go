//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
)

// A file far over the argument size limit is written whole, to a path with a
// space in it, an empty file is written empty, and listing a directory that
// is not there is ErrFileNotFound.
func TestIntegration_Files_LargeWriteAndMissingDir(t *testing.T) {
	eng := newIntegrationEngine(t)
	ctx := context.Background()

	sb, err := eng.CreateSandbox(ctx, runtime.SandboxConfig{})
	require.NoError(t, err)

	content := make([]byte, 8<<20)
	_, err = rand.Read(content)
	require.NoError(t, err)
	sum := sha256.Sum256(content)

	path := "/tmp/deck files/big deck.pdf"
	require.NoError(t, eng.WriteFile(ctx, sb.ID, path, content))

	result, err := eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"sha256sum", path}})
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	assert.Equal(t, hex.EncodeToString(sum[:]), strings.Fields(result.Stdout)[0])

	require.NoError(t, eng.WriteFile(ctx, sb.ID, "/tmp/empty.txt", nil))
	result, err = eng.Exec(ctx, sb.ID, runtime.ExecOpts{Cmd: []string{"stat", "-c", "%s", "/tmp/empty.txt"}})
	require.NoError(t, err)
	require.Equal(t, 0, result.ExitCode, result.Stderr)
	assert.Equal(t, "0", strings.TrimSpace(result.Stdout))

	_, err = eng.ListDir(ctx, sb.ID, "/tmp/not-there")
	require.ErrorIs(t, err, runtime.ErrFileNotFound)
}
