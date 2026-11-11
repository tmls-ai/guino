package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
)

const testBridge = "br-abc123456789"

// recorder captures the invocations as "name args..." and fails the ones that
// contain failOn. `iptables -S <chain>` answers from listing, or from later
// (one entry per call, the last one repeating) when set.
type recorder struct {
	calls   []string
	stdin   []string
	failOn  []string
	listing map[string]string
	later   map[string][]string
	missing string // a binary that is not installed
}

func (r *recorder) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	r.stdin = append(r.stdin, stdin)
	if name == r.missing {
		return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	for _, f := range r.failOn {
		if strings.Contains(line, f) {
			return "", errors.New("iptables: failed")
		}
	}
	if len(args) == 2 && args[0] == "-S" {
		if seq := r.later[args[1]]; len(seq) > 0 {
			out := seq[0]
			if len(seq) > 1 {
				r.later[args[1]] = seq[1:]
			}
			return out, nil
		}
		return r.listing[args[1]], nil
	}
	return "", nil
}

var (
	userJump  = "-A " + netpolicy.DockerUserChain + " -j " + netpolicy.EgressChainName()
	inputJump = "-A INPUT -i " + testBridge + " -j " + netpolicy.EgressHostChainName()
	hooked    = map[string]string{
		netpolicy.DockerUserChain: "-N DOCKER-USER\n" + userJump + "\n-A DOCKER-USER -j RETURN\n",
		"INPUT":                   "-P INPUT ACCEPT\n" + inputJump + "\n",
	}
)

// applyRules drives the production install with a recorder in place of
// iptables; only the bridge lookup, which needs a daemon, is skipped.
func applyRules(t *testing.T, rec *recorder) error {
	t.Helper()
	rules, err := netpolicy.EgressRules(netpolicy.EgressDesktop, testBridge, nil)
	if err != nil {
		t.Fatalf("building rules: %v", err)
	}
	return installEgressRules(context.Background(), rules, testBridge, rec.run, slog.New(slog.DiscardHandler))
}

// Both chains go in through one iptables-restore commit before any jump is
// checked, so a hooked chain is never half filled.
func TestInstallRestoresThenHooks(t *testing.T) {
	rec := &recorder{listing: hooked}
	if err := applyRules(t, rec); err != nil {
		t.Fatalf("clean install refused: %v", err)
	}
	if rec.calls[0] != "iptables-restore --noflush" {
		t.Fatalf("first call must be the atomic restore, got %q", rec.calls[0])
	}
	if !strings.Contains(rec.stdin[0], "-d 169.254.0.0/16 -j DROP") || !strings.HasSuffix(rec.stdin[0], "COMMIT\n") {
		t.Errorf("restore input is not the ruleset:\n%s", rec.stdin[0])
	}
	// Already first in both chains: only listed, never touched. The bridge
	// isolation table is replaced last.
	last := len(rec.calls) - 1
	if rec.calls[last] != "nft -f -" || !strings.Contains(rec.stdin[last], "meta protocol != { ip, arp } drop") {
		t.Errorf("bridge isolation not installed last: %q\n%s", rec.calls[last], rec.stdin[last])
	}
	for _, c := range rec.calls[1:last] {
		if !strings.HasPrefix(c, "iptables -S ") {
			t.Errorf("a jump already in place must be left alone, got %q", c)
		}
	}
}

// A missing jump is inserted first in its chain.
func TestMissingHookIsInserted(t *testing.T) {
	rec := &recorder{}
	if err := applyRules(t, rec); err != nil {
		t.Fatalf("first install refused: %v", err)
	}
	want := []string{
		"iptables -I " + netpolicy.DockerUserChain + " 1 -j " + netpolicy.EgressChainName(),
		"iptables -I INPUT 1 -i " + testBridge + " -j " + netpolicy.EgressHostChainName(),
	}
	for _, w := range want {
		found := false
		for _, c := range rec.calls {
			found = found || c == w
		}
		if !found {
			t.Errorf("missing %q in %v", w, rec.calls)
		}
	}
}

// A jump that something else got in front of is bypassed: it is put first again,
// then the old copy is removed by its (shifted) number.
func TestHookBehindAnotherRuleIsMovedFirst(t *testing.T) {
	before := "-N DOCKER-USER\n-A DOCKER-USER -j ACCEPT\n" + userJump + "\n"
	after := "-N DOCKER-USER\n" + userJump + "\n-A DOCKER-USER -j ACCEPT\n" + userJump + "\n"
	rec := &recorder{listing: hooked, later: map[string][]string{netpolicy.DockerUserChain: {before, after}}}
	if err := applyRules(t, rec); err != nil {
		t.Fatalf("install refused: %v", err)
	}
	got := strings.Join(rec.calls, "|")
	want := "iptables -I DOCKER-USER 1 -j " + netpolicy.EgressChainName() + "|iptables -S DOCKER-USER|iptables -D DOCKER-USER 3"
	if !strings.Contains(got, want) {
		t.Fatalf("want %q in %v", want, rec.calls)
	}

	// Someone else changed the chain in between: line 3 is no longer our jump,
	// so nothing is deleted by number.
	moved := "-N DOCKER-USER\n" + userJump + "\n-A DOCKER-USER -j DROP\n-A DOCKER-USER -j ACCEPT\n" + userJump + "\n"
	rec = &recorder{listing: hooked, later: map[string][]string{netpolicy.DockerUserChain: {before, moved}}}
	if err := applyRules(t, rec); err != nil {
		t.Fatalf("install refused: %v", err)
	}
	for _, c := range rec.calls {
		if strings.HasPrefix(c, "iptables -D ") {
			t.Fatalf("deleted %q although the chain changed underneath", c)
		}
	}
}

// A failed restore or insert must refuse startup. A ruleset that half-installed
// looks exactly like a working one until someone reaches the router.
func TestRealFailureRefusesStartup(t *testing.T) {
	for _, failing := range [][]string{
		{"iptables-restore"},
		{"iptables -S " + netpolicy.DockerUserChain},
		{"iptables -I " + netpolicy.DockerUserChain},
		{"iptables -I INPUT"},
		{"nft -f -"},
	} {
		rec := &recorder{failOn: failing}
		err := applyRules(t, rec)
		if err == nil {
			t.Errorf("startup continued after %v failed; the profile is not actually installed", failing)
			continue
		}
		if !strings.Contains(err.Error(), "refuses to start") {
			t.Errorf("refusal for %v did not use the operator message: %v", failing, err)
		}
	}
}

// The open profile is the default and must touch nothing at all, including on a
// machine with no iptables, which is every developer's Mac.
func TestOpenProfileRunsNoCommands(t *testing.T) {
	rules, err := netpolicy.EgressRules(netpolicy.EgressOpen, testBridge, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("the open profile would have run %d commands", len(rules))
	}
}

// The rules are IPv4 only, so the profile turns IPv6 off on the bridge: a
// dual-stack API must not be reachable over the bridge's link-local address.
func TestDesktopProfileDisablesIPv6OnTheBridge(t *testing.T) {
	conf := t.TempDir()
	sw := filepath.Join(conf, testBridge, "disable_ipv6")
	if err := os.MkdirAll(filepath.Dir(sw), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sw, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := disableBridgeIPv6(conf, testBridge); err != nil {
		t.Fatalf("disabling IPv6: %v", err)
	}
	if got, _ := os.ReadFile(sw); strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("disable_ipv6 = %q, want 1", got)
	}

	// A kernel without IPv6 has nothing to turn off.
	if err := disableBridgeIPv6(filepath.Join(conf, "absent"), testBridge); err != nil {
		t.Fatalf("no IPv6 in the kernel must not refuse: %v", err)
	}
	// IPv6 present but the switch cannot be set: refuse.
	if err := disableBridgeIPv6(conf, "br-missing"); err == nil {
		t.Fatal("a bridge whose IPv6 could not be turned off must refuse")
	}
}

// A desktop VM without nft keeps starting (with a loud log); nft that is
// present but rejects the ruleset still refuses.
func TestMissingNftWarnsButFailingNftRefuses(t *testing.T) {
	if err := applyRules(t, &recorder{listing: hooked, missing: "nft"}); err != nil {
		t.Fatalf("a host without nft must still start: %v", err)
	}
	if err := applyRules(t, &recorder{listing: hooked, failOn: []string{"nft -f -"}}); err == nil {
		t.Fatal("nft that fails to load the ruleset must refuse")
	}
}

// On a kernel without nft_meta_bridge the ibrname rule answers "Operation not
// supported": the port-name rule goes in instead. Any other nft error, and a
// fallback that fails too, still refuse.
func TestBridgeIsolationFallsBackWithoutMetaBridge(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	rules, err := netpolicy.EgressRules(netpolicy.EgressDesktop, testBridge, nil)
	if err != nil {
		t.Fatal(err)
	}
	install := func(failIbrname, failPorts string) (*recorder, error) {
		rec := &recorder{listing: hooked}
		run := func(ctx context.Context, stdin, name string, args ...string) (string, error) {
			out, err := rec.run(ctx, stdin, name, args...)
			if name == "nft" && strings.Contains(stdin, "ibrname") && failIbrname != "" {
				return "", errors.New("nft -f -: exit status 1: Error: Could not process rule: " + failIbrname)
			}
			if name == "nft" && strings.Contains(stdin, `iifname "veth*"`) && failPorts != "" {
				return "", errors.New("nft -f -: exit status 1: Error: " + failPorts)
			}
			return out, err
		}
		return rec, installEgressRules(context.Background(), rules, testBridge, run, logger)
	}

	rec, err := install("Operation not supported", "")
	if err != nil {
		t.Fatalf("the port-name fallback must start: %v", err)
	}
	last := len(rec.stdin) - 1
	if rec.calls[last] != "nft -f -" || !strings.Contains(rec.stdin[last], `iifname "veth*" oifname "veth*"`) {
		t.Fatalf("fallback not installed last: %q\n%s", rec.calls[last], rec.stdin[last])
	}

	if _, err := install("No such file or directory", ""); err == nil {
		t.Fatal("an nft error other than unsupported must refuse, not fall back")
	}
	if _, err := install("Operation not supported", "syntax error"); err == nil {
		t.Fatal("a failing fallback must refuse")
	}
	if rec, err := install("", ""); err != nil || strings.Contains(strings.Join(rec.stdin, ""), `iifname "veth*"`) {
		t.Fatalf("with nft_meta_bridge the ibrname rule alone must go in: %v", err)
	}
}
