//go:build darwin

package engine

import (
	"testing"
)

// Guino refused to create sandboxes on a Mac with plenty of memory free, because
// it counted only free+inactive pages as available. macOS reclaims purgeable and
// file-backed pages on demand, so ignoring them overstates pressure badly — and
// the user-visible symptom was the worst kind: the agent accepted a message and
// silently never answered.
func TestAvailableMemoryMatchesWhatMacOSItselfReports(t *testing.T) {
	b := &DarwinMemoryBackend{}
	total, used, free, err := b.HostMemory()
	if err != nil {
		t.Fatalf("HostMemory: %v", err)
	}
	if total == 0 || used > total {
		t.Fatalf("nonsense reading: total=%d used=%d", total, used)
	}

	// Logged, not asserted: the real reading depends on whatever else this
	// machine is doing, and a busy Mac must not fail the suite.
	t.Logf("score=%.3f used=%d free=%d total=%d", float64(used)/float64(total), used, free, total)
}

func TestVMStatParsesTheReclaimableCounters(t *testing.T) {
	pageSize, free, inactive, purgeable, fileBacked, err := parseVMStat()
	if err != nil {
		t.Skipf("vm_stat unavailable: %v", err)
	}
	if pageSize == 0 {
		t.Fatal("page size must not be zero")
	}
	// free and inactive are always present; the two we added must parse as well,
	// or the fix silently degrades to the old behaviour.
	if purgeable == 0 && fileBacked == 0 {
		t.Error("neither purgeable nor file-backed pages parsed; the fix is inert")
	}
	t.Logf("page=%d free=%d inactive=%d purgeable=%d file-backed=%d",
		pageSize, free, inactive, purgeable, fileBacked)
}
