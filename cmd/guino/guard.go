package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	goruntime "runtime"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

// startupSteps are the ordered, fail-fast startup actions shared verbatim by
// `serve` and `mcp`. They are passed as function values so the
// security-critical ordering — network-policy guards run and abort BEFORE any
// network mutation (Reconcile/EnsureNetwork) — is unit-testable in
// guard_test.go without a live Docker daemon.
type startupSteps struct {
	ping      func(context.Context) error
	guard     func(context.Context) error
	reconcile func(context.Context) error
	ensureNet func(context.Context) error
	// egress runs last: the rules name the managed network's bridge interface,
	// which does not exist until ensureNet has run. Optional — nil means the
	// entrypoint installs no egress profile.
	egress func(context.Context) error
}

// runStartup executes the startup steps in the one order that is safe: Ping →
// guards → Reconcile → EnsureNetwork → egress. A guard refusal MUST short-circuit
// before Reconcile/EnsureNetwork so a refused posture never mutates the
// managed network. Error wrapping is identical to the previous inline code in
// both entrypoints.
func runStartup(ctx context.Context, s startupSteps) error {
	if err := s.ping(ctx); err != nil {
		return fmt.Errorf("docker not available: %w", err)
	}
	if err := s.guard(ctx); err != nil {
		return err
	}
	if err := s.reconcile(ctx); err != nil {
		return fmt.Errorf("reconciling Guino network: %w", err)
	}
	if err := s.ensureNet(ctx); err != nil {
		return fmt.Errorf("ensuring Guino network: %w", err)
	}
	if s.egress != nil {
		if err := s.egress(ctx); err != nil {
			return err
		}
	}
	return nil
}

// platformProbe collapses the three live-RPC platform inputs
// (SystemInfo + DaemonHost + client GOOS → ClassifyPlatform) behind a single
// injectable seam so the positive platform_override bind-guard branch is
// provable in a hermetic unit test with a fake probe — no live Docker daemon,
// no SKIP-on-darwin. A non-nil error means the topology is indeterminate
// (daemon unreachable); the wrapper maps that to PlatformUnknown WITHOUT
// calling ClassifyPlatform, preserving the original guard.go clause-(a)
// semantics exactly. This is the precise case platform_override exists for.
type platformProbe func(context.Context, *docker.DockerRuntime) (netpolicy.RuntimePlatform, error)

// realPlatformProbe is the verbatim lift of the former inline clause-(a) block
// (SystemInfo → ClassifyPlatform(info, DaemonHost, GOOS)). It is the SOLE
// probe passed at the single production call site of
// applyNetworkGuardsWithProbe, by the bare identifier `realPlatformProbe`.
//
// SECURITY-INVARIANT: guard_ast_test.go parses every non-_test.go file in
// cmd/guino and enforces three equalities — (ii-a) the wrapper's probe argument
// is the exact identifier `realPlatformProbe`, (ii-b) that name resolves to
// exactly one top-level FuncDecl and is never a binding occurrence, and
// (ii-c) the PlatformLinuxNativeDocker assignment carries the same-line
// //guino:attested-platform-assignment marker — so the pinned spelling is the
// pinned value by construction. Deleting or renaming this function, or
// substituting a stub at the call site, fails that test. Do not remove the
// marker comment below or the AST exemption stops covering this assignment.
func realPlatformProbe(ctx context.Context, rt *docker.DockerRuntime) (netpolicy.RuntimePlatform, error) {
	info, err := rt.SystemInfo(ctx)
	if err != nil {
		return netpolicy.PlatformUnknown, err
	}
	return netpolicy.ClassifyPlatform(info, rt.DaemonHost(), goruntime.GOOS), nil
}

// applyNetworkGuards runs the startup network-policy guards. It MUST be called
// after rt.Ping and BEFORE rt.EnsureNetwork/Reconcile, in both `serve` and
// `mcp` mode.
//
// It is a thin caller of applyNetworkGuardsWithProbe wired to the real probe.
// This is the ONLY production call site of the wrapper, and the probe argument
// is the bare identifier `realPlatformProbe` — see the SECURITY-INVARIANT note
// on realPlatformProbe and guard_ast_test.go.
func applyNetworkGuards(ctx context.Context, rt *docker.DockerRuntime, cfg *config.Config, logger *slog.Logger, httpListener bool) error {
	return applyNetworkGuardsWithProbe(ctx, rt, cfg, logger, httpListener, realPlatformProbe)
}

// hostBindRoots converts the configured roots to the policy type.
func hostBindRoots(cfg *config.Config) []netpolicy.HostBindRoot {
	roots := make([]netpolicy.HostBindRoot, len(cfg.Sandbox.HostBindRoots))
	for i, r := range cfg.Sandbox.HostBindRoots {
		roots[i] = netpolicy.HostBindRoot(r)
	}
	return roots
}

// hostBindGuard refuses to start when host binds are enabled in a posture where
// anything reaching the control plane could ask for a directory of the host
// filesystem. It runs alongside the network guards for the same reason they
// exist: a refusal has to happen before anything is created, not at first use.
func hostBindGuard(cfg *config.Config, logger *slog.Logger, httpListener bool) error {
	if !cfg.Sandbox.AllowHostBinds {
		return nil
	}
	if httpListener && !netpolicy.HostBindsAllowed(cfg.Auth.Enabled, cfg.Server.Host) {
		logger.Error(netpolicy.MsgHostBindsUnsafe,
			"auth_enabled", cfg.Auth.Enabled, "server_host", cfg.Server.Host)
		return errors.New(netpolicy.MsgHostBindsUnsafe)
	}
	// A catastrophic root is a refusal to start, not a surprise at first bind.
	if err := netpolicy.ValidateHostBindRoots(hostBindRoots(cfg), filepath.EvalSymlinks); err != nil {
		logger.Error("host bind roots are not usable", "error", err)
		return err
	}
	if len(cfg.Sandbox.HostBindRoots) == 0 {
		logger.Warn("allow_host_binds is true but no host_bind_roots are configured; every bind request will be refused")
	} else {
		for _, root := range cfg.Sandbox.HostBindRoots {
			logger.Info("host bind root", "path", root.Path, "read_only", root.ReadOnly)
		}
	}
	return nil
}

// applyNetworkGuardsWithProbe is applyNetworkGuards with the platform probe
// injected, so guard_test.go can prove the positive platform_override branch
// (override attested ⇒ Guino STARTS + committed ERROR attestation logged) and
// the refusal/probe-error branches deterministically with a fake probe.
//
// httpListener reports whether this process exposes the HTTP control plane
// (true for `serve`, false for MCP stdio mode). The bind guard — and its
// security-critical ERROR-level opt-in disclosures, which describe the HTTP
// control-plane exposure — are a no-op without an HTTP listener. The
// bridge-refusal guard and the non-fatal Warnings ALWAYS run: a bridge
// sandbox has unfiltered egress regardless of whether Guino exposes an HTTP API.
//
// On refusal it writes the committed netpolicy message to stderr and returns a
// non-nil error so the caller exits non-zero.
func applyNetworkGuardsWithProbe(ctx context.Context, rt *docker.DockerRuntime, cfg *config.Config, logger *slog.Logger, httpListener bool, probe platformProbe) error {
	if err := hostBindGuard(cfg, logger, httpListener); err != nil {
		return err
	}
	effectiveMode := netpolicy.Policy{
		DefaultMode: runtime.NetworkMode(cfg.Runtime.DefaultNetworkMode),
	}.EffectiveDefault()

	// Bridge-refusal guard: runs in BOTH HTTP and MCP mode, and aborts FIRST.
	if netpolicy.BridgeRefusalDecision(effectiveMode, cfg.Runtime.AllowUnsafeBridge) {
		fmt.Fprintln(os.Stderr, netpolicy.MsgBridgeRefusal)
		return errors.New("refusing to start: runtime.default_network_mode=bridge without runtime.allow_unsafe_bridge")
	}

	attested := netpolicy.PlatformOverrideAttested(cfg.Runtime.PlatformOverride)

	if httpListener {
		// Clause (a): an indeterminate probe (failed `docker info`) maps to
		// PlatformUnknown WITHOUT calling ClassifyPlatform — the fail-closed
		// default. ClassifyPlatform is only reached inside realPlatformProbe
		// after a successful SystemInfo, so an error here can never have
		// consulted it.
		platform := netpolicy.PlatformUnknown
		if p, err := probe(ctx, rt); err == nil {
			platform = p
		}
		// v9 §4(a): platform_override forces runtimePlatform=linux-native-docker
		// for the BindGuardDecision input ONLY. It is deliberately NOT written
		// back to `platform`; no reconcile/IPv6/future path may consume the
		// override as a true platform fact.
		guardPlatform := platform
		if attested {
			guardPlatform = netpolicy.PlatformLinuxNativeDocker //guino:attested-platform-assignment
		}
		safe := netpolicy.BindGuardDecision(
			cfg.Auth.Enabled,
			netpolicy.ClassifyHost(cfg.Server.Host),
			effectiveMode,
			guardPlatform,
			attested,
			cfg.Runtime.AllowUnsafeBind,
		)
		if !safe {
			fmt.Fprintln(os.Stderr, netpolicy.MsgBindRefusal)
			return errors.New("refusing to start: unauthenticated HTTP control plane reachable from sandboxes")
		}

		// Security-critical opt-in disclosures: ERROR-level, every start.
		// Scoped to the HTTP listener — the messages describe the HTTP
		// control-plane exposure, which does not exist in MCP stdio mode.
		if cfg.Runtime.AllowUnsafeBind {
			logger.Error(netpolicy.MsgUnsafeBindEnabled)
		}
		if attested {
			logger.Error(netpolicy.MsgPlatformOverrideAttested)
		}
	}

	// Non-fatal advisories (docker_host inert, internal-not-a-boundary).
	for _, line := range cfg.Warnings() {
		logger.Warn(line)
	}
	return nil
}
