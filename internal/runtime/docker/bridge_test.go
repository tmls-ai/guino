package docker

import "testing"

// The egress rules are scoped to this name; a wrong one matches nothing and
// blocks nothing.
func TestBridgeInterfaceName(t *testing.T) {
	got, err := bridgeInterfaceName("0123456789abcdef0123", nil)
	if err != nil || got != "br-0123456789ab" {
		t.Fatalf("derived name = %q, %v", got, err)
	}
	got, err = bridgeInterfaceName("0123456789abcdef0123", map[string]string{"com.docker.network.bridge.name": "docker0"})
	if err != nil || got != "docker0" {
		t.Fatalf("explicit name = %q, %v", got, err)
	}
	if _, err := bridgeInterfaceName("short", nil); err == nil {
		t.Fatal("a malformed id must be refused")
	}
}
