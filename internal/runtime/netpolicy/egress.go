package netpolicy

import (
	"fmt"
	"strconv"
	"strings"
)

// Egress filtering for the desktop profile.
//
// Why this exists: `bridge` mode gives a sandbox the host's own view of the
// network. On a laptop that is the user's home LAN — the router's admin page, a
// NAS, a printer, a colleague's machine, and the cloud metadata address on any
// VM. Guino does not program the *host's* firewall by default and says so
// (SECURITY.md §(8)), and that stays true for the "open" profile.
// "desktop" is meant for a dedicated sandbox VM, where Guino is root, the Docker
// daemon is local, and the rules only touch traffic from the managed bridge.
//
// The shape is deliberately a denylist of address space rather than an allowlist
// of destinations. The agent's whole value is reaching arbitrary internet hosts,
// so an allowlist would either be useless or maintained forever; what must be
// unreachable is a small, fixed, well-known set of ranges.
//
// Everything here is pure: rules are produced as argument vectors and returned,
// never executed. The wiring that runs them lives with the runtime, and the
// tests below need no root, no iptables and no daemon.

// EgressProfile names a set of egress rules.
type EgressProfile string

const (
	// EgressOpen installs nothing: the historical behaviour, for a server
	// operator who has their own firewall around the machine.
	EgressOpen EgressProfile = "open"

	// EgressDesktop blocks private, link-local and other special-purpose
	// address space while permitting ordinary internet egress.
	EgressDesktop EgressProfile = "desktop"
)

// DockerUserChain is the chain Docker guarantees it will jump to first and will
// not rewrite. Rules placed anywhere else are removed the next time Docker
// regenerates its own chains (a container start is enough).
const DockerUserChain = "DOCKER-USER"

// hostChain drops sandbox traffic addressed to the VM itself.
//
// This is a separate chain because it hooks a different table path. DOCKER-USER
// is reached from FORWARD, which only sees traffic *through* the host. A packet
// from a container to its own bridge gateway is addressed to a local interface,
// so it goes to INPUT and never passes DOCKER-USER at all — measured, not
// assumed: with only the FORWARD rules installed, a sandbox could still fetch
// Guino's own /api/v1/health on the gateway address.
//
// That path is the escalation SECURITY.md warns about (a sandbox reaching the
// Guino API via the network gateway). Authentication is the primary control and
// stays mandatory; this closes the network route as well.
const hostChain = "GUINO-EGRESS-HOST"

// denChain holds Guino's forward-path rules. A dedicated chain rather than rules
// appended directly to DOCKER-USER, so a reinstall can flush exactly these rules
// without disturbing anything else the operator put there.
const denChain = "GUINO-EGRESS"

// MsgEgressUnavailable is emitted (and the process exits non-zero) when the
// desktop profile is requested but the rules cannot be installed. Failing open
// here would be the worst outcome: the operator believes the LAN is unreachable
// while the agent can browse to the router.
const MsgEgressUnavailable = "guino refuses to start: runtime.egress_profile=\"desktop\" was requested but the egress rules could not be installed. " +
	"This profile only works where guino is root in the same network namespace as the Docker bridge (for example inside a dedicated sandbox VM). " +
	"Remediations: run guino inside the VM as root; or set runtime.egress_profile=\"open\" and accept that sandboxes can reach the local network"

// blockedV4 is the address space a desktop sandbox must not reach.
//
// Chosen from what is actually dangerous on an end user's machine rather than
// from RFC1918 alone:
//
//	10/8, 172.16/12, 192.168/16   private LANs — the router, NAS, printers, and
//	                              the Docker bridge itself, so a sandbox cannot
//	                              reach Guino's control plane or a sibling network
//	100.64/10                     CGNAT, which some ISP routers use for the LAN
//	169.254/16                    link-local, and with it 169.254.169.254 — the
//	                              cloud metadata address, relevant because the
//	                              same image runs on a server
//	127/8                         loopback, via a spoofed destination
//	0/8, 224/4, 240/4             this-network, multicast, reserved
//
// Not included: 192.0.2/24, 198.51.100/24 and 203.0.113/24 (documentation
// ranges) — they route nowhere and blocking them buys nothing.
var blockedV4 = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"169.254.0.0/16",
	"127.0.0.0/8",
	"0.0.0.0/8",
	"224.0.0.0/4",
	"240.0.0.0/4",
}

// EgressRules returns the iptables invocations that install `profile` for
// traffic leaving containers on `bridge`, in the order they must be run.
//
// resolver is the VM's own DNS server, exempted (DNS only) from the denylist it
// would otherwise fall inside; empty adds no exemption. controlPort is Guino's
// own listening port (server.port), the one thing on the gateway a sandbox must
// never reach. It is taken from the running config rather than assumed, since a
// mismatch would silently reopen the hole.
//
// The rules are the chains' complete contents ("-A <chain> ..."). They are
// installed with EgressRestore and EgressHooks, never one by one: a chain that
// is flushed and refilled rule by rule is hooked but half empty in between, and
// a restart (or a crash mid-install) would leave running sandboxes unfiltered.
//
// Returns nil for EgressOpen: "open" touches nothing. Switching a machine from
// "desktop" back to "open" therefore leaves the chains in place (fail closed);
// remove them by hand or reboot the VM.
func EgressRules(profile EgressProfile, bridge, resolver string, controlPort int) ([][]string, error) {
	if profile == EgressOpen {
		return nil, nil
	}
	if profile != EgressDesktop {
		return nil, fmt.Errorf("unknown egress profile %q", profile)
	}
	if bridge == "" {
		return nil, fmt.Errorf("egress profile %q needs the bridge interface name", profile)
	}
	if controlPort < 1 || controlPort > 65535 {
		return nil, fmt.Errorf("egress profile %q needs the control port, got %d", profile, controlPort)
	}
	port := strconv.Itoa(controlPort)

	forward := func(chain string) [][]string {
		rules := [][]string{
			// Replies to connections opened *toward* a sandbox must survive. Only
			// the reply direction: a connection a sandbox opened to the LAN before
			// the rules existed must not be grandfathered in.
			{"-A", chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "--ctdir", "REPLY", "-j", "RETURN"},
		}
		// The VM's own resolver, exempted before the denylist.
		//
		// Measured, not anticipated: with the ranges below and nothing else, the
		// counter on the 192.168/16 DROP showed 6 packets / 366 bytes and the
		// sandbox had no DNS. The hypervisor's NAT puts the VM (and the resolver
		// it hands to containers) inside 192.168.64.0/24, the same range that
		// blocks the user's home router.
		//
		// Scoped to one address and to DNS only, so "the user's router is
		// unreachable" still holds: a router at 192.168.1.1 is not this VM's
		// gateway, and even the gateway is reachable for nothing but name
		// resolution.
		if resolver != "" {
			for _, proto := range []string{"udp", "tcp"} {
				rules = append(rules, []string{
					"-A", chain, "-i", bridge, "-d", resolver,
					"-p", proto, "--dport", "53", "-j", "RETURN",
				})
			}
		}
		for _, cidr := range blockedV4 {
			rules = append(rules, []string{"-A", chain, "-i", bridge, "-d", cidr, "-j", "DROP"})
		}
		// Anything else falls back to Docker's own decision.
		return append(rules, []string{"-A", chain, "-j", "RETURN"})
	}

	// Traffic to the host itself takes the INPUT path, which DOCKER-USER never
	// sees.
	//
	// Port-scoped, not address-scoped. Dropping everything from the bridge also
	// drops the gateway, and the gateway is the sandbox's first hop for the
	// internet, so a blanket rule takes away the network it was supposed to leave
	// alone. Measured: with the blanket rule a sandbox's default route
	// (172.18.0.1) became unreachable and it had no internet at all, while the
	// VM itself did. What must stay unreachable is Guino's control plane on any
	// local address, not the address.
	host := func(chain string) [][]string {
		return [][]string{
			// Replies to connections the host opened toward a sandbox (the
			// control plane proxying a preview, for one) must survive.
			{"-A", chain, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "--ctdir", "REPLY", "-j", "RETURN"},
			{"-A", chain, "-i", bridge, "-p", "tcp", "--dport", port, "-j", "DROP"},
			{"-A", chain, "-i", bridge, "-p", "udp", "--dport", port, "-j", "DROP"},
			{"-A", chain, "-j", "RETURN"},
		}
	}

	return append(forward(denChain), host(hostChain)...), nil
}

// EgressRestore renders rules as input for `iptables-restore --noflush`. Each
// declared chain is created if missing and its contents replaced in a single
// commit: a reader sees the old ruleset or the new one, never a partial one, and
// a rejected input leaves the old one in place.
func EgressRestore(rules [][]string) string {
	var b strings.Builder
	b.WriteString("*filter\n:" + denChain + " - [0:0]\n:" + hostChain + " - [0:0]\n")
	for _, r := range rules {
		b.WriteString(strings.Join(r, " ") + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// EgressHooks returns the jumps into the chains as `<builtin> <match...>`. Each
// is checked with `iptables -C` and inserted at position 1 only when missing, so
// an existing jump is never removed and re-added (which would be a gap).
// DOCKER-USER may hold operator rules; the jump must precede them to matter.
func EgressHooks(bridge string) [][]string {
	return [][]string{
		{DockerUserChain, "-j", denChain},
		{"INPUT", "-i", bridge, "-j", hostChain},
	}
}

// EgressChainName exposes Guino's forward-path chain for tests and for operators
// reading `iptables -L`.
func EgressChainName() string { return denChain }

// EgressHostChainName exposes the INPUT-path chain, which is the one that stops
// a sandbox from reaching Guino itself on the bridge gateway.
func EgressHostChainName() string { return hostChain }

// BlockedEgressCIDRs returns the blocked address space, for tests and for the
// startup log line that tells an operator what was installed.
func BlockedEgressCIDRs() []string {
	out := make([]string, len(blockedV4))
	copy(out, blockedV4)
	return out
}
