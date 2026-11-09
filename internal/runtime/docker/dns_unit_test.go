package docker

import (
	"os"
	"path/filepath"
	"testing"
)

// runtime.sandbox_dns is written to a file mounted read-only over
// /etc/resolv.conf; without servers nothing is written or mounted.
func TestSandboxDNSWritesAndMountsResolvConf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guino-resolv.conf")
	r, err := New(WithDNS([]string{"1.1.1.1", "172.31.0.2"}, path))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "nameserver 1.1.1.1\nnameserver 172.31.0.2\n" {
		t.Fatalf("resolv.conf = %q", got)
	}
	m := withResolvConf(nil, r.resolvConf)
	if len(m) != 1 || m[0].Source != path || m[0].Target != "/etc/resolv.conf" || !m[0].ReadOnly {
		t.Fatalf("mount = %+v", m)
	}

	unset := filepath.Join(t.TempDir(), "unused.conf")
	r, err = New(WithDNS(nil, unset))
	if err != nil {
		t.Fatal(err)
	}
	if len(withResolvConf(nil, r.resolvConf)) != 0 {
		t.Fatal("no servers must add no mount")
	}
	if _, err := os.Stat(unset); !os.IsNotExist(err) {
		t.Fatal("no servers must write no file")
	}
}

// The file is replaced by rename, never rewritten in place: a sandbox that
// has it mounted keeps the complete old file.
func TestResolvConfIsReplacedNotRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guino-resolv.conf")
	if _, err := New(WithDNS([]string{"1.1.1.1"}, path)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(WithDNS([]string{"8.8.8.8"}, path)); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("resolv.conf was rewritten in place")
	}
	if got, _ := os.ReadFile(path); string(got) != "nameserver 8.8.8.8\n" {
		t.Fatalf("resolv.conf = %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".guino-resolv-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}
