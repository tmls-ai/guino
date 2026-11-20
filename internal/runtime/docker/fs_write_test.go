package docker

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// WriteFile's command runs as is in a local shell: a file far over the
// argument size limit arrives whole, and a path no shell should parse is
// written where it says.
func TestWriteFileCmdTakesContentOnStdin(t *testing.T) {
	content := make([]byte, 6<<20)
	_, err := rand.Read(content)
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, `it's a "deck" $(touch pwned) ; x.pdf`)

	cmd := writeFileCmd(path)
	run := exec.Command(cmd[0], cmd[1:]...) // #nosec G204 -- the command under test
	run.Dir = dir
	run.Stdin = bytes.NewReader(content)
	out, err := run.CombinedOutput()
	require.NoError(t, err, string(out))

	got, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	require.NoError(t, err)
	require.True(t, bytes.Equal(content, got), "content changed on the way")
	_, err = os.Stat(filepath.Join(dir, "pwned"))
	require.True(t, os.IsNotExist(err), "the path was run as shell code")
}

func TestWriteFileCmdWritesAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	cmd := writeFileCmd(path)
	run := exec.Command(cmd[0], cmd[1:]...) // #nosec G204 -- the command under test
	run.Stdin = bytes.NewReader(nil)
	require.NoError(t, run.Run())
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Zero(t, info.Size())
}
