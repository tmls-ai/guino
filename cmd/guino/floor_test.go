package main

import (
	"flag"
	"fmt"
	"os"
	"sync"
	"testing"
)

// This file is the cmd/guino package-local named-proof FLOOR. testing.M.Run()
// returns only an exit code, never which tests ran, so a build that silently
// skips/excludes the four hermetic platform_override proofs (build-tag drift,
// a stray `t.Skip`, a deleted test that still leaves the rest of cmd/guino
// green) would exit 0. TestMain asserts, by NAME, that each required proof
// executed.
//
// This is a TRUE NAMED-SET floor, NOT the netpolicytest count model. A
// reviewer correctly flagged that netpolicytest.Mark only increments an
// int64 and never records t.Name(): a bare counter would still pass if any
// OTHER cmd/guino test ran after a proof was deleted — that is a count floor
// mislabeled "named". Here mark(t) records t.Name() into `seen` and TestMain
// asserts the SPECIFIC required names are present, so deleting any one fails
// loudly even with the rest of cmd/guino green.
//
// runFilterActive() is intentionally a ~6-line copy of
// netpolicytest/mark.go's unexported helper (cross-referenced here). The
// floor is inert under an explicit -run/-skip because the operator then
// deliberately selected a subset (e.g. `make test-integration` runs
// `-run TestIntegration`, matching zero cmd/guino proofs) — enforcing it there
// would be a false failure. A tiny independent name-set is lower-risk than
// widening netpolicytest's API or coupling two unrelated package floors by
// reading its shared counter (different package, different suite).

// requiredProofs are the four hermetic platform_override proofs that MUST
// have executed on an unfiltered `go test ./cmd/guino/...`.
var requiredProofs = []string{
	"TestApplyNetworkGuards_PositiveOverrideAttested",
	"TestApplyNetworkGuards_RefusalWithoutOverride",
	"TestApplyNetworkGuards_ProbeErrorAttestedStarts",
	"TestApplyNetworkGuards_ProbeErrorNoOverrideRefuses",
}

var seen sync.Map // test name -> struct{}

// mark records that a required proof executed. Call it as the first line of
// each proof.
func mark(t *testing.T) {
	t.Helper()
	seen.Store(t.Name(), struct{}{})
}

// runFilterActive reports whether an explicit -run/-skip selection is active.
// Verbatim copy of netpolicytest/mark.go:26 (see file comment for why this is
// duplicated rather than exported). Called after m.Run(), so flags are parsed.
func runFilterActive() bool {
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	code := m.Run()

	if !runFilterActive() {
		var missing []string
		for _, name := range requiredProofs {
			if _, ok := seen.Load(name); !ok {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(os.Stderr,
				"cmd/guino named-proof floor NOT met: these required platform_override "+
					"proofs did not execute: %v (build-tag drift, a stray t.Skip, or a "+
					"deleted proof — failing loudly)\n", missing)
			if code == 0 {
				code = 1
			}
		}
	}

	os.Exit(code)
}
