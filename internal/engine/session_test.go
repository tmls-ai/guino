package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
)

func TestValidateSessionName(t *testing.T) {
	ok := []string{"supervisord-session", "a", "A1_b-2", "x_" + "y"}
	for _, n := range ok {
		if err := ValidateSessionName(n); err != nil {
			t.Fatalf("expected %q to be valid, got %v", n, err)
		}
	}
	// A name becomes part of a command line, so anything that could break out
	// must be refused rather than escaped.
	bad := []string{"", "a b", "a;rm -rf /", "$(id)", "`id`", "a\nb", "a'b", `a"b`, "../x", string(make([]byte, 65))}
	for _, n := range bad {
		if err := ValidateSessionName(n); err == nil {
			t.Fatalf("expected %q to be rejected", n)
		}
	}
}

// tmux treats a bare -t name as a prefix match, so every call must target the
// session exactly, and the command must reach the shell byte for byte: not read
// as key names, and not cut at a trailing ";" that tmux would take as its own
// separator (`find ... -exec ... \;`).
func TestSessionCommandsTargetExactlyAndSendLiterally(t *testing.T) {
	eng, mock := newTestEngine(t)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{})
	require.NoError(t, err)

	var mu sync.Mutex
	var calls [][]string
	mock.ExecFn = func(_ context.Context, _ string, opts runtime.ExecOpts) (*runtime.ExecResult, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, opts.Cmd)
		return &runtime.ExecResult{ExitCode: 0}, nil
	}

	cmd := `find . -exec echo {} \;`
	require.NoError(t, eng.SessionExec(context.Background(), sb.ID, "web", cmd))
	_, err = eng.SessionLogs(context.Background(), sb.ID, "web", 0)
	require.NoError(t, err)
	require.NoError(t, eng.DeleteSession(context.Background(), sb.ID, "web"))

	var sawSend bool
	for _, c := range calls {
		for i, a := range c {
			if a == "-t" {
				assert.Equal(t, "=web:", c[i+1], "non-exact target in %v", c)
			}
		}
		if len(c) > 1 && c[1] == "send-keys" {
			sawSend = true
			assert.Equal(t, []string{"tmux", "send-keys", "-t", "=web:", "-l", "--", cmd + "\n"}, c)
		}
	}
	assert.True(t, sawSend)
}

func TestSessionErrorsAreTyped(t *testing.T) {
	eng, mock := newTestEngine(t)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{})
	require.NoError(t, err)

	mock.ExecFn = func(_ context.Context, _ string, opts runtime.ExecOpts) (*runtime.ExecResult, error) {
		if strings.Join(opts.Cmd, " ") == "tmux has-session -t =web:" {
			return &runtime.ExecResult{ExitCode: 1}, nil
		}
		return &runtime.ExecResult{ExitCode: 0}, nil
	}
	err = eng.SessionExec(context.Background(), sb.ID, "web", "ls")
	assert.True(t, errors.Is(err, ErrSessionNotFound), "got %v", err)

	err = eng.SessionExec(context.Background(), sb.ID, "web", "  ")
	assert.True(t, errors.Is(err, ErrInvalidSession), "got %v", err)

	mock.ExecFn = nil // every call exits 0, so has-session finds it
	err = eng.CreateSession(context.Background(), sb.ID, "web")
	assert.True(t, errors.Is(err, ErrSessionExists), "got %v", err)
}

// An image without tmux answers every session call the same way, not just create.
func TestSessionWithoutTmuxIsUnsupported(t *testing.T) {
	eng, mock := newTestEngine(t)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{})
	require.NoError(t, err)
	mock.ExecFn = func(context.Context, string, runtime.ExecOpts) (*runtime.ExecResult, error) {
		return &runtime.ExecResult{ExitCode: 127, Stderr: "tmux: not found"}, nil
	}
	_, err = eng.ListSessions(context.Background(), sb.ID)
	assert.True(t, errors.Is(err, ErrSessionUnsupported), "list: %v", err)
	err = eng.SessionExec(context.Background(), sb.ID, "web", "ls")
	assert.True(t, errors.Is(err, ErrSessionUnsupported), "exec: %v", err)
	err = eng.DeleteSession(context.Background(), sb.ID, "web")
	assert.True(t, errors.Is(err, ErrSessionUnsupported), "delete: %v", err)
}
