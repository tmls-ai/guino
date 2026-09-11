package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- guard-ordering seam -----------------------------------------------------
//
// The one safe order is Ping → guards → Reconcile → EnsureNetwork. A guard
// refusal MUST short-circuit before Reconcile/EnsureNetwork so a refused
// posture never mutates the managed network. These tests pin that with spies
// instead of a live daemon.

func TestRunStartup_OrderHappyPath(t *testing.T) {
	var order []string
	step := func(name string) func(context.Context) error {
		return func(context.Context) error { order = append(order, name); return nil }
	}
	err := runStartup(context.Background(), startupSteps{
		ping:      step("ping"),
		guard:     step("guard"),
		reconcile: step("reconcile"),
		ensureNet: step("ensureNet"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"ping", "guard", "reconcile", "ensureNet"}, order,
		"startup must run in exactly this order")
}

func TestRunStartup_GuardRefusalAbortsBeforeMutation(t *testing.T) {
	var order []string
	step := func(name string) func(context.Context) error {
		return func(context.Context) error { order = append(order, name); return nil }
	}
	sentinel := errors.New("refusing to start: bind guard")
	err := runStartup(context.Background(), startupSteps{
		ping:      step("ping"),
		guard:     func(context.Context) error { order = append(order, "guard"); return sentinel },
		reconcile: step("reconcile"),
		ensureNet: step("ensureNet"),
	})
	require.ErrorIs(t, err, sentinel, "guard error must propagate unwrapped")
	assert.Equal(t, []string{"ping", "guard"}, order,
		"a guard refusal must abort BEFORE Reconcile/EnsureNetwork")
}

func TestRunStartup_PingFailureAbortsBeforeGuard(t *testing.T) {
	var order []string
	step := func(name string) func(context.Context) error {
		return func(context.Context) error { order = append(order, name); return nil }
	}
	err := runStartup(context.Background(), startupSteps{
		ping:      func(context.Context) error { order = append(order, "ping"); return errors.New("no daemon") },
		guard:     step("guard"),
		reconcile: step("reconcile"),
		ensureNet: step("ensureNet"),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "docker not available")
	assert.Equal(t, []string{"ping"}, order, "a ping failure must abort before the guard runs")
}

// --- bridge-refusal runs in MCP mode (httpListener=false) --------------------
//
// MCP is stdio-only so the bind guard is a no-op, but a bridge sandbox has
// unfiltered egress regardless of whether Guino exposes an HTTP API. The
// bridge-refusal guard MUST still fire. docker.New() is lazy (no daemon
// contacted) and the bridge check returns before rt is ever touched.

func TestApplyNetworkGuards_BridgeRefusalRunsInMCP(t *testing.T) {
	rt, err := docker.New()
	require.NoError(t, err)

	cfg := config.DefaultConfig()
	cfg.Runtime.DefaultNetworkMode = "bridge"
	cfg.Runtime.AllowUnsafeBridge = false
	// httpListener=false ⇒ MCP stdio mode.
	err = applyNetworkGuards(context.Background(), rt, cfg, quietLogger(), false)
	require.Error(t, err, "bridge without allow_unsafe_bridge must refuse even in MCP mode")
	assert.Contains(t, err.Error(), "bridge")

	// Opt-in flips it to allowed; the bridge guard no longer refuses.
	cfg.Runtime.AllowUnsafeBridge = true
	require.NoError(t, applyNetworkGuards(context.Background(), rt, cfg, quietLogger(), false))
}

func TestApplyNetworkGuards_BindGuardIsNoOpInMCP(t *testing.T) {
	rt, err := docker.New()
	require.NoError(t, err)

	// Auth off + loopback bind + internal default would REFUSE under an HTTP
	// listener; in MCP stdio mode the bind guard must NOT run, so this starts.
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = false
	cfg.Server.Host = "127.0.0.1"
	cfg.Runtime.DefaultNetworkMode = "internal"
	require.NoError(t, applyNetworkGuards(context.Background(), rt, cfg, quietLogger(), false),
		"the bind guard must be a no-op in MCP stdio mode")
}

// --- MCP is stdio-only: structural no-network-listener invariant -------------
//
// mcp.go must never open a network listener. This is enforced structurally
// (go/parser) rather than by hoping a reviewer notices, plus a comment
// tripwire so deleting the rationale also fails the build.

func TestMCP_IsStdioOnly_Structural(t *testing.T) {
	src, err := os.ReadFile("mcp.go")
	require.NoError(t, err)

	// Tripwire: the SECURITY-INVARIANT rationale must stay in the file.
	assert.Contains(t, string(src), "SECURITY-INVARIANT: mcp mode is stdio-only",
		"the no-listener rationale comment was removed from mcp.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mcp.go", src, parser.ParseComments)
	require.NoError(t, err)

	// (1) mcp.go must not even import a network-server package.
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		switch path {
		case "net", "net/http", "net/http/httptest":
			t.Fatalf("mcp.go must not import %q — MCP is stdio-only", path)
		}
	}

	// (2) Defense-in-depth: no listener/server selector even via a dot-aliased
	// or transitively reachable identifier.
	netForbidden := map[string]bool{
		"ListenAndServe": true, "ListenAndServeTLS": true,
		"Serve": true, "ServeTLS": true, "Server": true, "FileServer": true,
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch pkg.Name {
		case "net":
			if strings.HasPrefix(sel.Sel.Name, "Listen") {
				t.Fatalf("mcp.go references net.%s — MCP must not open a listener", sel.Sel.Name)
			}
		case "http":
			if netForbidden[sel.Sel.Name] {
				t.Fatalf("mcp.go references http.%s — MCP must not run an HTTP server", sel.Sel.Name)
			}
		case "httptest":
			t.Fatalf("mcp.go references httptest.%s — MCP must not open a listener", sel.Sel.Name)
		}
		return true
	})
}

// --- hermetic positive platform_override proof ------------------------------
//
// These four named proofs are the ENVIRONMENT-INDEPENDENT, deterministic
// replacement for the retired dind refusal class and the SKIP-on-darwin Leg D.
// They inject a fake platformProbe into applyNetworkGuardsWithProbe (the seam
// applyNetworkGuards is a thin caller of) so the positive override-attested
// bind-guard branch and the indeterminate-probe branches are provable with NO
// live Docker daemon. floor_test.go asserts all four ran by name.
//
// Assertion-channel correctness: MsgPlatformOverrideAttested is emitted via
// logger.Error, so a capturing slog.Handler is the right probe for the STARTS
// cases; MsgBindRefusal is written with fmt.Fprintln(os.Stderr, …) and the
// RETURNED error is a different literal — so the refusal proofs assert
// require.Error + the returned-error substring AND captured stderr containing
// MsgBindRefusal, never a slog record (a slog assertion of refusal would be a
// vacuous always-pass).

// recordingHandler captures slog records so an ERROR-level message can be
// asserted by exact text and level.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) hasErrorMessage(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == slog.LevelError && r.Message == msg {
			return true
		}
	}
	return false
}

// captureStderr redirects os.Stderr to a pipe and returns a one-shot reader
// that restores the original. t.Cleanup restores it even if the reader is
// never called, so there is no leakage across the four proofs.
func captureStderr(t *testing.T) (read func() string) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	done := false
	t.Cleanup(func() {
		if !done {
			os.Stderr = orig
			_ = w.Close()
			_ = r.Close()
		}
	})
	return func() string {
		_ = w.Close()
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		_ = r.Close()
		os.Stderr = orig
		done = true
		return buf.String()
	}
}

// guardBindActiveConfig returns a config whose bind guard WOULD refuse under
// an HTTP listener (auth off + loopback bind + non-`none` default mode) so the
// only thing that can let Guino start is an attested platform_override.
func guardBindActiveConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Auth.Enabled = false
	cfg.Server.Host = "127.0.0.1"
	cfg.Runtime.DefaultNetworkMode = "internal"
	cfg.Runtime.AllowUnsafeBind = false
	require.True(t, netpolicy.ClassifyHost(cfg.Server.Host) == netpolicy.HostLoopback,
		"precondition: 127.0.0.1 must classify as loopback")
	return cfg
}

func fakeProbe(p netpolicy.RuntimePlatform, err error) platformProbe {
	return func(context.Context, *docker.DockerRuntime) (netpolicy.RuntimePlatform, error) {
		return p, err
	}
}

// TestApplyNetworkGuards_PositiveOverrideAttested: a healthy probe +
// attested override + loopback bind + auth off + httpListener=true ⇒ Guino
// STARTS and logs MsgPlatformOverrideAttested at ERROR. This is the positive
// branch that was previously only provable in a real native-Linux topology.
func TestApplyNetworkGuards_PositiveOverrideAttested(t *testing.T) {
	mark(t)
	cfg := guardBindActiveConfig(t)
	cfg.Runtime.PlatformOverride = netpolicy.PlatformOverrideCoResident

	h := &recordingHandler{}
	err := applyNetworkGuardsWithProbe(
		context.Background(), nil, cfg, slog.New(h), true,
		fakeProbe(netpolicy.PlatformLinuxNativeDocker, nil),
	)

	require.NoError(t, err, "attested override on a loopback auth-off host MUST start")
	assert.True(t, h.hasErrorMessage(netpolicy.MsgPlatformOverrideAttested),
		"the committed platform-override attestation MUST be logged at ERROR on every start")
}

// TestApplyNetworkGuards_RefusalWithoutOverride: same posture MINUS the
// override ⇒ Guino REFUSES. Asserts the returned-error literal AND that the
// committed MsgBindRefusal reached stderr (not slog).
func TestApplyNetworkGuards_RefusalWithoutOverride(t *testing.T) {
	mark(t)
	cfg := guardBindActiveConfig(t) // no PlatformOverride

	readStderr := captureStderr(t)
	err := applyNetworkGuardsWithProbe(
		context.Background(), nil, cfg, quietLogger(), true,
		fakeProbe(netpolicy.PlatformLinuxNativeDocker, nil),
	)
	stderr := readStderr()

	require.Error(t, err, "loopback auth-off host WITHOUT override MUST refuse")
	assert.Contains(t, err.Error(),
		"refusing to start: unauthenticated HTTP control plane reachable from sandboxes",
		"the returned error must be the guard's bind-refusal literal")
	assert.Contains(t, stderr, netpolicy.MsgBindRefusal,
		"the committed MsgBindRefusal must be written to stderr")
}

// TestApplyNetworkGuards_ProbeErrorAttestedStarts: an INDETERMINATE probe
// (daemon unreachable) is exactly the case platform_override exists for.
// probe error + attested ⇒ Guino STARTS + ERROR attestation. The indeterminate
// probe must NOT silently downgrade/override the operator's override.
func TestApplyNetworkGuards_ProbeErrorAttestedStarts(t *testing.T) {
	mark(t)
	cfg := guardBindActiveConfig(t)
	cfg.Runtime.PlatformOverride = netpolicy.PlatformOverrideCoResident

	h := &recordingHandler{}
	err := applyNetworkGuardsWithProbe(
		context.Background(), nil, cfg, slog.New(h), true,
		fakeProbe(netpolicy.PlatformUnknown, errors.New("docker info: connection refused")),
	)

	require.NoError(t, err,
		"an indeterminate probe must not override an attested platform_override")
	assert.True(t, h.hasErrorMessage(netpolicy.MsgPlatformOverrideAttested),
		"attestation MUST still be logged when the probe was indeterminate")
}

// TestApplyNetworkGuards_ProbeErrorNoOverrideRefuses: indeterminate probe
// WITHOUT override ⇒ refuse (indeterminate is unsafe by default). Makes "an
// unreachable daemon silently downgrades the guard" a failing test.
func TestApplyNetworkGuards_ProbeErrorNoOverrideRefuses(t *testing.T) {
	mark(t)
	cfg := guardBindActiveConfig(t) // no PlatformOverride

	readStderr := captureStderr(t)
	err := applyNetworkGuardsWithProbe(
		context.Background(), nil, cfg, quietLogger(), true,
		fakeProbe(netpolicy.PlatformUnknown, errors.New("docker info: connection refused")),
	)
	stderr := readStderr()

	require.Error(t, err, "indeterminate probe WITHOUT override MUST refuse")
	assert.Contains(t, err.Error(),
		"refusing to start: unauthenticated HTTP control plane reachable from sandboxes")
	assert.Contains(t, stderr, netpolicy.MsgBindRefusal,
		"the committed MsgBindRefusal must be written to stderr")
}
