package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/tmls-ai/guino/internal/runtime"
)

// Sessions are long-lived shells inside a sandbox.
//
// Guino's Exec is one-shot: every call is its own process, so nothing survives
// between calls — not a working directory, not an environment variable, not a
// server you started. Agents need the opposite: run a dev server, then check on
// it; start a build, then read its output. That is what a session is.
//
// The implementation is tmux inside the sandbox rather than a long-lived
// connection held by Guino. tmux already solves the hard parts (detached
// processes, scrollback, a name to reattach by) and it survives a Guino restart,
// which a connection could not. The cost is a dependency on tmux being present
// in the image; images that lack it get a clear error rather than silent
// misbehaviour.

// sessionNamePattern constrains names to what is safe to interpolate into the
// shell command below. Anything else is rejected rather than escaped: a name is
// caller-supplied and this string becomes part of a command line.
var sessionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ErrSessionUnsupported is returned when the sandbox image has no tmux.
var ErrSessionUnsupported = errors.New("sandbox image does not provide tmux")

// ErrSessionNotFound and ErrSessionExists let the API answer 404 and 409.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrSessionExists   = errors.New("session already exists")
)

// ErrInvalidSession is a malformed name or an empty command, a 400.
var ErrInvalidSession = errors.New("invalid session request")

// ValidateSessionName reports whether a session name is usable.
func ValidateSessionName(name string) error {
	if !sessionNamePattern.MatchString(name) {
		return fmt.Errorf("%w: session name must match %s", ErrInvalidSession, sessionNamePattern.String())
	}
	return nil
}

// sessionExec runs one command in the sandbox. Exit 127 is the shell's
// "command not found": the image has no tmux, whatever the call was.
func (e *Engine) sessionExec(ctx context.Context, id string, args ...string) (*runtime.ExecResult, error) {
	res, err := e.Exec(ctx, id, runtime.ExecOpts{Cmd: args})
	if err == nil && res.ExitCode == 127 {
		return nil, ErrSessionUnsupported
	}
	return res, err
}

// sessionTarget is the tmux target for exactly this session. A bare name is a
// prefix match in tmux: "-t web" would hit "web2" when "web" does not exist, and
// send keys to or kill the wrong session. "=" forces an exact match.
func sessionTarget(name string) string { return "=" + name + ":" }

// CreateSession starts a detached tmux session in the sandbox.
func (e *Engine) CreateSession(ctx context.Context, id, name string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}

	// Fail with a useful message rather than a confusing "command not found"
	// buried in the first exec's stderr.
	if probe, err := e.sessionExec(ctx, id, "sh", "-c", "command -v tmux >/dev/null 2>&1"); err != nil {
		return err
	} else if probe.ExitCode != 0 {
		return ErrSessionUnsupported
	}

	if res, err := e.sessionExec(ctx, id, "tmux", "has-session", "-t", sessionTarget(name)); err == nil && res.ExitCode == 0 {
		return fmt.Errorf("%w: %q", ErrSessionExists, name)
	}

	res, err := e.sessionExec(ctx, id, "tmux", "new-session", "-d", "-s", name)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		// Lost a race with a concurrent create of the same name.
		if strings.Contains(res.Stderr, "duplicate session") {
			return fmt.Errorf("%w: %q", ErrSessionExists, name)
		}
		return fmt.Errorf("tmux new-session failed: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// ListSessions returns the tmux session names in the sandbox.
func (e *Engine) ListSessions(ctx context.Context, id string) ([]string, error) {
	res, err := e.sessionExec(ctx, id, "tmux", "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}
	// tmux exits non-zero with "no server running" when nothing was ever
	// started. That is an empty list, not an error.
	if res.ExitCode != 0 {
		return []string{}, nil
	}
	names := []string{}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// SessionExec sends a command to a session. It returns immediately: the command
// keeps running in the session, and output is read back with SessionLogs. This
// is the asynchronous behaviour agents rely on for servers and long builds.
func (e *Engine) SessionExec(ctx context.Context, id, name, command string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}
	if strings.TrimSpace(command) == "" {
		return fmt.Errorf("%w: command is required", ErrInvalidSession)
	}

	if res, err := e.sessionExec(ctx, id, "tmux", "has-session", "-t", sessionTarget(name)); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, name)
	}

	// The command is one literal argument and the sandbox's shell parses it,
	// exactly as if typed. -l stops tmux reading it as key names ("Enter",
	// "C-c"), and the trailing newline submits it in the same call so two
	// concurrent execs cannot interleave. It also keeps the argument from ending
	// in ";", which tmux would otherwise take as its own command separator and
	// strip, breaking `find ... -exec ... \;`. "--" keeps a command that starts
	// with "-" from being read as a flag.
	res, err := e.sessionExec(ctx, id, "tmux", "send-keys", "-t", sessionTarget(name), "-l", "--", command+"\n")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tmux send-keys failed: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// SessionLogs returns the session's scrollback, newest content last.
func (e *Engine) SessionLogs(ctx context.Context, id, name string, lines int) (string, error) {
	if err := ValidateSessionName(name); err != nil {
		return "", err
	}
	if lines <= 0 {
		lines = 500
	}

	res, err := e.sessionExec(ctx, id,
		"tmux", "capture-pane", "-p", "-t", sessionTarget(name), "-S", fmt.Sprintf("-%d", lines))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("%w: %q", ErrSessionNotFound, name)
	}
	return res.Stdout, nil
}

// DeleteSession kills a session and everything running in it.
func (e *Engine) DeleteSession(ctx context.Context, id, name string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}
	res, err := e.sessionExec(ctx, id, "tmux", "kill-session", "-t", sessionTarget(name))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w: %q", ErrSessionNotFound, name)
	}
	return nil
}
