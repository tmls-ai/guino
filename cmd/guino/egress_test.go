package main

import (
	"context"
	"errors"
	"log/slog"
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
}

func (r *recorder) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, line)
	r.stdin = append(r.stdin, stdin)
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
	rules, err := netpolicy.EgressRules(netpolicy.EgressDesktop, testBridge, "", 8080)
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
	// Already first in both chains: only listed, never touched.
	for _, c := range rec.calls[1:] {
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
	rules, err := netpolicy.EgressRules(netpolicy.EgressOpen, testBridge, "", 8080)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("the open profile would have run %d commands", len(rules))
	}
}
