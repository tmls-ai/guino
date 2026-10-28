package netpolicy

import (
	"net"
	"strings"
	"testing"
)

const testControlPort = 8080

func rulesFor(t *testing.T, profile EgressProfile, bridge string) [][]string {
	t.Helper()
	rules, err := EgressRules(profile, bridge, "", testControlPort)
	if err != nil {
		t.Fatalf("EgressRules(%q, %q): %v", profile, bridge, err)
	}
	return rules
}

func joined(rules [][]string) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = strings.Join(r, " ")
	}
	return out
}

func TestOpenProfileInstallsNothing(t *testing.T) {
	rules, err := EgressRules(EgressOpen, "br-abc", "", testControlPort)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("the open profile must install nothing, got %v", joined(rules))
	}
}

func TestUnknownProfileIsAnError(t *testing.T) {
	if _, err := EgressRules("paranoid", "br-abc", "", testControlPort); err == nil {
		t.Fatal("an unknown profile must be an error, not a silent no-op")
	}
	// A missing bridge name would produce rules that match every interface on the
	// box, including the VM's own uplink. That must fail, not install.
	if _, err := EgressRules(EgressDesktop, "", "", testControlPort); err == nil {
		t.Fatal("an empty bridge name must be refused")
	}
}

// The addresses that actually matter on an end user's machine. If any of these
// stops being covered, the agent can reach the user's router or a cloud
// metadata endpoint.
func TestDangerousAddressesAreCovered(t *testing.T) {
	mustBlock := []string{
		"192.168.1.1",     // the home router's admin page
		"192.168.0.254",   // ditto, other common default
		"10.0.0.1",        // corporate LAN gateway
		"172.17.0.1",      // the Docker bridge gateway, Guino's own control plane
		"172.19.0.2",      // a sibling sandbox
		"169.254.169.254", // cloud instance metadata
		"100.64.0.1",      // CGNAT, used as LAN space by some ISP routers
		"127.0.0.1",       // loopback via a spoofed destination
		"224.0.0.1",       // multicast
	}
	mustReach := []string{
		"1.1.1.1",
		"8.8.8.8",
		"140.82.121.4",  // github
		"151.101.1.140", // a CDN
		"172.15.255.255",
		"172.32.0.1", // just outside 172.16/12 — the classic off-by-one
		"11.0.0.1",   // just outside 10/8
		"100.63.255.255",
		"100.128.0.1", // just outside 100.64/10
	}

	var nets []*net.IPNet
	for _, cidr := range BlockedEgressCIDRs() {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatalf("blocked CIDR %q does not parse: %v", cidr, err)
		}
		nets = append(nets, n)
	}
	covered := func(ip string) bool {
		parsed := net.ParseIP(ip)
		for _, n := range nets {
			if n.Contains(parsed) {
				return true
			}
		}
		return false
	}

	for _, ip := range mustBlock {
		if !covered(ip) {
			t.Errorf("%s is reachable from a sandbox; it must not be", ip)
		}
	}
	for _, ip := range mustReach {
		if covered(ip) {
			t.Errorf("%s is blocked; ordinary internet egress must keep working", ip)
		}
	}
}

// Order inside a chain: a DROP that precedes the conntrack RETURN would kill
// replies, and the catch-all RETURN must come last or nothing is blocked.
func TestRuleOrder(t *testing.T) {
	rules := joined(rulesFor(t, EgressDesktop, "br-guino"))

	idx := func(substr string) int {
		for i, r := range rules {
			if strings.Contains(r, substr) {
				return i
			}
		}
		t.Fatalf("no rule containing %q in %v", substr, rules)
		return -1
	}

	live := "-A " + denChain + " "
	conntrack := idx(live + "-m conntrack")
	firstDrop := idx(live + "-i br-guino -d 10.0.0.0/8")
	catchAll := idx(live + "-j RETURN")
	if conntrack >= firstDrop || firstDrop >= catchAll {
		t.Errorf("want conntrack RETURN, then DROPs, then catch-all RETURN: %v", rules)
	}
	if !strings.Contains(rules[conntrack], "--ctdir REPLY") {
		t.Errorf("only replies may skip the denylist: %q", rules[conntrack])
	}
}

// The restore input declares both chains (iptables-restore --noflush then
// replaces exactly their contents in one commit) and carries every rule once.
func TestRestoreReplacesBothChainsAtomically(t *testing.T) {
	rules := rulesFor(t, EgressDesktop, "br-guino")
	in := EgressRestore(rules)
	if !strings.HasPrefix(in, "*filter\n:"+denChain+" - [0:0]\n:"+hostChain+" - [0:0]\n") {
		t.Fatalf("both chains must be declared up front:\n%s", in)
	}
	if !strings.HasSuffix(in, "COMMIT\n") {
		t.Fatalf("the input must end in a single COMMIT:\n%s", in)
	}
	if got := strings.Count(in, "\n-A "); got != len(rules) {
		t.Errorf("restore carries %d rules, want %d", got, len(rules))
	}
	for _, r := range rules {
		if r[0] != "-A" || (r[1] != denChain && r[1] != hostChain) {
			t.Errorf("only appends to Guino's own chains may go in the commit: %v", r)
		}
	}
}

// Jumps go first in DOCKER-USER (ahead of operator rules) and INPUT is scoped to
// the bridge.
func TestHooks(t *testing.T) {
	got := EgressHooks("br-guino")
	want := [][]string{
		{DockerUserChain, "-j", denChain},
		{"INPUT", "-i", "br-guino", "-j", hostChain},
	}
	if strings.Join(joined(got), "|") != strings.Join(joined(want), "|") {
		t.Errorf("hooks = %v, want %v", got, want)
	}
}

// Every DROP is scoped to the bridge. Without -i, the same rule would also drop
// the VM's own traffic to those ranges, which includes whatever the host uses to
// talk to the VM.
func TestDropsAreScopedToTheBridge(t *testing.T) {
	for _, rule := range rulesFor(t, EgressDesktop, "br-guino") {
		line := strings.Join(rule, " ")
		if !strings.Contains(line, "-j DROP") {
			continue
		}
		if !strings.Contains(line, "-i br-guino") {
			t.Errorf("unscoped DROP would affect the whole VM: %q", line)
		}
	}
}

// The refusal message is an operator contract, like the bind guard's.
func TestRefusalMessageNamesTheFixes(t *testing.T) {
	for _, want := range []string{"egress_profile", "root", "VM", "open"} {
		if !strings.Contains(MsgEgressUnavailable, want) {
			t.Errorf("the refusal message does not mention %q: %s", want, MsgEgressUnavailable)
		}
	}
}

// The host-path rule must be port-scoped. A blanket drop from the bridge also
// drops the gateway, which is the sandbox's first hop for the internet — measured
// on a real VM, where the sandbox's default route became unreachable and it had
// no network at all while the VM itself was online.
func TestHostChainDropsOnlyTheControlPort(t *testing.T) {
	rules := joined(rulesFor(t, EgressDesktop, "br-guino"))

	var hostDrops []string
	for _, r := range rules {
		if strings.Contains(r, hostChain) && strings.Contains(r, "-j DROP") {
			hostDrops = append(hostDrops, r)
		}
	}
	if len(hostDrops) == 0 {
		t.Fatal("nothing stops a sandbox reaching Guino on the gateway")
	}
	for _, r := range hostDrops {
		if !strings.Contains(r, "--dport 8080") {
			t.Errorf("blanket drop would take the gateway with it: %q", r)
		}
	}
	// Both transports, or the hole is open on one of them.
	var tcp, udp bool
	for _, r := range hostDrops {
		tcp = tcp || strings.Contains(r, "-p tcp")
		udp = udp || strings.Contains(r, "-p udp")
	}
	if !tcp || !udp {
		t.Errorf("control port must be blocked on tcp and udp: %v", hostDrops)
	}
}

// The VM's own resolver has to be exempted, and exempted narrowly.
//
// Found by packet counter, not by reading: the 192.168/16 DROP showed 6 packets /
// 366 bytes and the sandbox had no DNS. The hypervisor's NAT puts the VM and the
// resolver it hands out inside 192.168.64.0/24 — the same range that blocks the
// user's home router.
func TestResolverIsExemptedForDNSOnly(t *testing.T) {
	rules, err := EgressRules(EgressDesktop, "br-guino", "192.168.64.1", testControlPort)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := joined(rules)

	var exempt []string
	for _, r := range lines {
		if strings.HasPrefix(r, "-A "+denChain+" ") && strings.Contains(r, "192.168.64.1") {
			exempt = append(exempt, r)
		}
	}
	if len(exempt) != 2 {
		t.Fatalf("expected a udp and a tcp DNS exemption, got %v", exempt)
	}
	for _, r := range exempt {
		if !strings.Contains(r, "--dport 53") {
			t.Errorf("the resolver exemption must be DNS-only, got %q", r)
		}
		if !strings.Contains(r, "-j RETURN") {
			t.Errorf("the exemption must RETURN, not ACCEPT: %q", r)
		}
	}

	// Order: the exemption is useless after the DROP that would have eaten it.
	idx := func(sub string) int {
		for i, r := range lines {
			if strings.Contains(r, sub) {
				return i
			}
		}
		t.Fatalf("no rule containing %q", sub)
		return -1
	}
	if idx("192.168.64.1") > idx("-d 192.168.0.0/16") {
		t.Error("the resolver exemption must precede the range that would drop it")
	}

	// And the property it must not weaken: a home router is still unreachable.
	for _, r := range lines {
		if strings.Contains(r, "192.168.1.1") {
			t.Errorf("the home router must not be exempted: %q", r)
		}
	}

	// No resolver: the plain denylist, unchanged.
	plain, _ := EgressRules(EgressDesktop, "br-guino", "", testControlPort)
	if len(plain) >= len(rules) {
		t.Error("an empty resolver should add no rules")
	}
}

// The control port comes from server.port. A hardcoded value would leave a
// server on any other port reachable from its own sandboxes.
func TestHostChainFollowsTheConfiguredPort(t *testing.T) {
	rules, err := EgressRules(EgressDesktop, "br-guino", "", 9090)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var drops int
	for _, r := range joined(rules) {
		if strings.HasPrefix(r, "-A "+hostChain+" ") && strings.Contains(r, "-j DROP") {
			drops++
			if !strings.Contains(r, "--dport 9090") {
				t.Errorf("host drop is not on the configured port: %q", r)
			}
		}
	}
	if drops != 2 {
		t.Errorf("expected tcp and udp drops on 9090, got %d", drops)
	}
	if _, err := EgressRules(EgressDesktop, "br-guino", "", 0); err == nil {
		t.Error("a missing control port must be refused")
	}
}
