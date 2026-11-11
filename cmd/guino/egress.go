package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

// Installing the desktop egress profile.
//
// The decision of *what* to block is pure and lives in netpolicy/egress.go; this
// file is only the part that has to touch the machine. It runs after the managed
// network exists, because the rules name that network's bridge interface.
//
// Failure is fatal by design. A profile that half-installs is worse than one that
// is absent: the operator believes the LAN is unreachable while the agent can
// still browse to the router.

// iptablesRunner executes one invocation of `iptables`, `iptables-restore` or `nft`
// with stdin as input. Injectable so the wiring is testable without root,
// without iptables and without a daemon.
type iptablesRunner func(ctx context.Context, stdin, name string, args ...string) (string, error)

func realIptables(ctx context.Context, stdin, name string, args ...string) (string, error) {
	// -w waits for the xtables lock instead of failing when Docker happens to be
	// writing rules at the same moment, which it does on every container start.
	// name is "iptables", "iptables-restore" or "nft" (which has no -w and
	// commits atomically) and the arguments come only from
	// netpolicy, never from a request.
	if name != "nft" {
		args = append([]string{"-w", "5"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed binaries, internally built args
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// bridgeInterface returns the managed network's bridge interface and checks it
// exists in this network namespace. iptables accepts rules naming an interface
// that is not there, and they would silently match nothing.
func bridgeInterface(ctx context.Context, rt *docker.DockerRuntime, networkID string) (string, error) {
	name, err := rt.BridgeInterface(ctx, networkID)
	if err != nil {
		return "", fmt.Errorf("inspecting network %s: %w", networkID, err)
	}
	if _, err := net.InterfaceByName(name); err != nil {
		return "", fmt.Errorf("bridge interface %s of network %s is not in this network namespace: %w", name, networkID, err)
	}
	return name, nil
}

// defaultGateway returns the VM's own gateway address, which is also the DNS
// server the hypervisor hands out. Parsed from `ip route` rather than a library:
// this runs inside our own minimal image where iproute2 is already a dependency.
func defaultGateway(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "via" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// applyEgressProfile installs the configured profile, or refuses to start.
func applyEgressProfile(
	ctx context.Context,
	rt *docker.DockerRuntime,
	cfg *config.Config,
	logger *slog.Logger,
	run iptablesRunner,
) error {
	profile := netpolicy.EgressProfile(cfg.Runtime.EgressProfile)
	if profile == "" {
		profile = netpolicy.EgressOpen
	}
	if profile == netpolicy.EgressOpen {
		return nil
	}

	bridge, err := bridgeInterface(ctx, rt, cfg.Runtime.NetworkID)
	if err != nil {
		logger.Error(netpolicy.MsgEgressUnavailable, "error", err)
		return errors.New(netpolicy.MsgEgressUnavailable)
	}

	// The configured sandbox DNS servers, or else the VM's own DNS server,
	// discovered from the default route: it is assigned by the hypervisor and
	// differs per platform and per boot. None is tolerated: the rules are then
	// the plain denylist, and the sandbox has no name resolution, which is
	// visible immediately.
	resolvers := cfg.Runtime.SandboxDNS
	if len(resolvers) == 0 {
		if gw := defaultGateway(ctx); gw != "" {
			resolvers = []string{gw}
		}
	}
	if len(resolvers) > 0 {
		logger.Info("exempting the sandbox resolvers from the egress denylist", "resolvers", strings.Join(resolvers, ","))
	}

	rules, err := netpolicy.EgressRules(profile, bridge, resolvers)
	if err != nil {
		return fmt.Errorf("building egress rules: %w", err)
	}
	if err := installEgressRules(ctx, rules, bridge, run, logger); err != nil {
		return err
	}
	if err := disableBridgeIPv6(ipv6ConfDir, bridge); err != nil {
		logger.Error(netpolicy.MsgEgressUnavailable, "step", "disable_ipv6", "error", err)
		return errors.New(netpolicy.MsgEgressUnavailable)
	}

	logger.Info("egress profile installed",
		"profile", profile,
		"bridge", bridge,
		"chain", netpolicy.EgressChainName(),
		"blocked", strings.Join(netpolicy.BlockedEgressCIDRs(), ","))
	return nil
}

// ipv6ConfDir holds the kernel's per-interface IPv6 switches.
const ipv6ConfDir = "/proc/sys/net/ipv6/conf"

// disableBridgeIPv6 turns IPv6 off on the bridge. The rules are IPv4 only, and
// the API may listen dual-stack: with IPv6 up on the bridge, a sandbox could
// reach the host over the bridge's link-local address and nothing would drop
// it. With it off the kernel discards every IPv6 packet arriving on the bridge,
// toward the host and through it. The managed network is IPv4 only, so nothing
// legitimate is lost. A kernel without IPv6 (no conf directory) needs nothing;
// a bridge that stays on is a refusal, like any rule that failed to install.
func disableBridgeIPv6(confDir, bridge string) error {
	if _, err := os.Stat(confDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	path := filepath.Join(confDir, bridge, "disable_ipv6")
	if cur, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(cur)) == "1" { //nolint:gosec // G304: fixed kernel path, bridge name checked against this namespace
		return nil
	}
	if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
		return fmt.Errorf("disabling IPv6 on %s: %w", bridge, err)
	}
	return nil
}

// installEgressRules replaces both chains in one iptables-restore commit, then
// makes each jump the first rule of its built-in chain. Neither step can leave a
// hooked chain half filled, so a restart never opens a window for running
// sandboxes. Any failure refuses startup.
func installEgressRules(ctx context.Context, rules [][]string, bridge string, run iptablesRunner, logger *slog.Logger) error {
	refuse := func(step string, err error) error {
		logger.Error(netpolicy.MsgEgressUnavailable, "step", step, "error", err)
		return errors.New(netpolicy.MsgEgressUnavailable)
	}
	if _, err := run(ctx, netpolicy.EgressRestore(rules), "iptables-restore", "--noflush"); err != nil {
		return refuse("iptables-restore", err)
	}
	for _, hook := range netpolicy.EgressHooks(bridge) {
		if err := hookFirst(ctx, hook, run, logger); err != nil {
			return refuse(strings.Join(hook, " "), err)
		}
	}
	isolation, err := netpolicy.BridgeIsolationNft(bridge)
	if err != nil {
		return refuse("nft", err)
	}
	if _, err := run(ctx, isolation, "nft", "-f", "-"); errors.Is(err, exec.ErrNotFound) {
		// ponytail: a single-user desktop VM may lack nft; a host shared by
		// several tenants must have it (the fleet AMI does).
		logger.Error("nft is not installed: sandboxes on this host are NOT isolated from each other over IPv6; install nftables on any host shared by more than one user", "bridge", bridge)
	} else if err != nil {
		return refuse("nft", err)
	}
	return nil
}

// hookFirst makes `hook` (built-in chain, then rule) the first rule of its
// chain. A jump that exists but sits behind something else (an ACCEPT someone
// inserted later) is bypassed, so it is re-inserted at 1 before the old copy is
// removed by number: there is never a moment without a jump.
func hookFirst(ctx context.Context, hook []string, run iptablesRunner, logger *slog.Logger) error {
	chain := hook[0]
	out, err := run(ctx, "", "iptables", "-S", chain)
	if err != nil {
		return err
	}
	want := "-A " + strings.Join(hook, " ")
	pos := firstIndex(rulesOf(out), want)
	if pos == 0 {
		return nil
	}
	if _, err := run(ctx, "", "iptables", append([]string{"-I", chain, "1"}, hook[1:]...)...); err != nil {
		return err
	}
	if pos > 0 {
		// The old copy moved down by one. Re-list first: if anything else
		// changed the chain meanwhile, a number could name another rule, so
		// only delete when that line is still our jump. A duplicate left
		// behind is harmless.
		if out, err := run(ctx, "", "iptables", "-S", chain); err == nil {
			if rules := rulesOf(out); len(rules) > pos+1 && rules[pos+1] == want {
				if _, err := run(ctx, "", "iptables", "-D", chain, strconv.Itoa(pos+2)); err != nil {
					logger.Warn("stale egress jump not removed", "chain", chain, "error", err)
				}
			}
		}
	}
	return nil
}

// rulesOf returns the "-A ..." lines of `iptables -S <chain>` output, in order.
func rulesOf(out string) []string {
	var rules []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "-A ") {
			rules = append(rules, line)
		}
	}
	return rules
}

func firstIndex(rules []string, want string) int {
	for i, r := range rules {
		if r == want {
			return i
		}
	}
	return -1
}
