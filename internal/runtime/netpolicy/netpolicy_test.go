package netpolicy_test

import (
	"os"
	"testing"

	"github.com/docker/docker/api/types/system"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy/netpolicytest"
)

// netpolicyTestFloor is the minimum number of suite tests that must execute.
// It is intentionally well below the real count so adding/removing a case
// never trips it; it only catches "the whole suite was skipped".
const netpolicyTestFloor = 25

func TestMain(m *testing.M) {
	code := m.Run()
	os.Exit(netpolicytest.AssertFloor(code, netpolicyTestFloor))
}

// --- ClassifyHost -----------------------------------------------------------

func TestClassifyHost(t *testing.T) {
	cases := []struct {
		host string
		want netpolicy.HostClass
	}{
		{"", netpolicy.HostNonLoopback},             // empty ⇒ fail-closed
		{"0.0.0.0", netpolicy.HostNonLoopback},      // wildcard
		{"::", netpolicy.HostNonLoopback},           // v6 wildcard
		{"::1", netpolicy.HostLoopback},             // v6 loopback
		{"127.0.0.1", netpolicy.HostLoopback},       // v4 loopback
		{"127.5.6.7", netpolicy.HostLoopback},       // 127/8 is all loopback
		{"localhost", netpolicy.HostNonLoopback},    // hostname, not an IP literal
		{"192.168.1.10", netpolicy.HostNonLoopback}, // LAN
		{"10.0.0.5", netpolicy.HostNonLoopback},     // LAN
		{"not-an-ip-!!", netpolicy.HostNonLoopback}, // unparseable ⇒ fail-closed
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			netpolicytest.Mark(t)
			assert.Equal(t, c.want, netpolicy.ClassifyHost(c.host))
		})
	}
}

// --- ClassifyPlatform (POSITIVE allowlist truth table) ----------------------

func linuxInfo() system.Info {
	return system.Info{
		OSType:          "linux",
		OperatingSystem: "Ubuntu 22.04",
		KernelVersion:   "6.5.0-generic",
		SecurityOptions: []string{"name=seccomp,profile=builtin"},
	}
}

func TestClassifyPlatform(t *testing.T) {
	cases := []struct {
		name       string
		info       func() system.Info
		dockerHost string
		clientGOOS string
		want       netpolicy.RuntimePlatform
	}{
		{
			name:       "native linux + linux client + unix socket ⇒ linux-native",
			info:       linuxInfo,
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformLinuxNativeDocker,
		},
		{
			name:       "empty daemon host (defensive-only) ⇒ local ⇒ linux-native",
			info:       linuxInfo,
			dockerHost: "",
			clientGOOS: "linux",
			want:       netpolicy.PlatformLinuxNativeDocker,
		},
		{
			name:       "custom unix path ⇒ local ⇒ linux-native",
			info:       linuxInfo,
			dockerHost: "unix:///run/user/1000/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformLinuxNativeDocker,
		},
		{
			name:       "darwin client + linux daemon via unix (Colima/OrbStack) ⇒ unknown",
			info:       linuxInfo,
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "darwin",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name:       "windows client ⇒ unknown",
			info:       linuxInfo,
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "windows",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name: "Docker Desktop ⇒ unknown",
			info: func() system.Info {
				i := linuxInfo()
				i.OperatingSystem = "Docker Desktop"
				return i
			},
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name: "linuxkit kernel ⇒ unknown",
			info: func() system.Info {
				i := linuxInfo()
				i.KernelVersion = "5.15.0-linuxkit"
				return i
			},
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name: "WSL2 kernel ⇒ unknown",
			info: func() system.Info {
				i := linuxInfo()
				i.KernelVersion = "5.15.90.1-microsoft-standard-WSL2"
				return i
			},
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name: "rootless security option ⇒ unknown",
			info: func() system.Info {
				i := linuxInfo()
				i.SecurityOptions = []string{"name=seccomp,profile=builtin", "name=rootless"}
				return i
			},
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name: "empty SecurityOptions parses ⇒ clause (f) passes",
			info: func() system.Info {
				i := linuxInfo()
				i.SecurityOptions = nil
				return i
			},
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformLinuxNativeDocker,
		},
		{
			name: "malformed SecurityOptions ⇒ unknown",
			info: func() system.Info {
				i := linuxInfo()
				// Has '=' so it enters the strict parser, but the second
				// comma-segment lacks its own '=' ⇒ DecodeSecurityOptions
				// errors ⇒ fail-closed.
				i.SecurityOptions = []string{"name=seccomp,malformed-no-equals"}
				return i
			},
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name: "non-linux OSType ⇒ unknown",
			info: func() system.Info {
				i := linuxInfo()
				i.OSType = "windows"
				return i
			},
			dockerHost: "unix:///var/run/docker.sock",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name:       "tcp:// daemon ⇒ remote ⇒ unknown",
			info:       linuxInfo,
			dockerHost: "tcp://docker:2375",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name:       "ssh:// daemon ⇒ remote ⇒ unknown",
			info:       linuxInfo,
			dockerHost: "ssh://user@host",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
		{
			name:       "npipe:// daemon ⇒ unknown",
			info:       linuxInfo,
			dockerHost: "npipe:////./pipe/docker_engine",
			clientGOOS: "linux",
			want:       netpolicy.PlatformUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			netpolicytest.Mark(t)
			got := netpolicy.ClassifyPlatform(c.info(), c.dockerHost, c.clientGOOS)
			assert.Equal(t, c.want, got)
		})
	}
}

// --- PlatformOverrideAttested ----------------------------------------------

func TestPlatformOverrideAttested(t *testing.T) {
	netpolicytest.Mark(t)
	assert.False(t, netpolicy.PlatformOverrideAttested(""))
	assert.False(t, netpolicy.PlatformOverrideAttested("yes"))
	assert.False(t, netpolicy.PlatformOverrideAttested("linux-native"))
	assert.True(t, netpolicy.PlatformOverrideAttested(netpolicy.PlatformOverrideCoResident))
	assert.Equal(t, "linux-native-docker-co-resident", netpolicy.PlatformOverrideCoResident)
}

// --- BindGuardDecision (v9 REFUSE-unless-attested truth table) --------------

func TestBindGuardDecision_v9KeyRows(t *testing.T) {
	const (
		authOn  = true
		authOff = false
		ovYes   = true
		ovNo    = false
		unsafe  = true
		safeOff = false
	)
	lb := netpolicy.HostLoopback
	nlb := netpolicy.HostNonLoopback
	lin := netpolicy.PlatformLinuxNativeDocker
	unk := netpolicy.PlatformUnknown
	internal := runtime.NetworkModeInternal
	none := runtime.NetworkModeNone

	cases := []struct {
		name string
		auth bool
		host netpolicy.HostClass
		mode runtime.NetworkMode
		plat netpolicy.RuntimePlatform
		ov   bool
		ub   bool
		want bool // safe?
	}{
		// THE v9 KEY ROWS:
		{"native-linux loopback auth-off NOT attested ⇒ REFUSE",
			authOff, lb, internal, lin, ovNo, safeOff, false},
		{"native-linux loopback auth-off attested ⇒ SAFE",
			authOff, lb, internal, lin, ovYes, safeOff, true},
		{"auth-on ⇒ SAFE regardless of attestation",
			authOn, lb, internal, lin, ovNo, safeOff, true},
		{"effective none ⇒ SAFE regardless of attestation",
			authOff, lb, none, lin, ovNo, safeOff, true},

		// Attestation must NOT rescue a non-loopback bind.
		{"non-loopback auth-off attested ⇒ REFUSE",
			authOff, nlb, internal, lin, ovYes, safeOff, false},
		// Attestation must NOT rescue an unknown platform.
		{"loopback auth-off unknown-platform attested ⇒ REFUSE",
			authOff, lb, internal, unk, ovYes, safeOff, false},
		// allow_unsafe_bind is the explicit last resort.
		{"allow_unsafe_bind ⇒ SAFE regardless",
			authOff, nlb, internal, unk, ovNo, unsafe, true},
		// non-loopback auth-on still safe (auth clause).
		{"non-loopback auth-on ⇒ SAFE",
			authOn, nlb, internal, unk, ovNo, safeOff, true},
		// the dangerous default: non-loopback, auth-off, nothing set.
		{"non-loopback auth-off nothing set ⇒ REFUSE",
			authOff, nlb, internal, unk, ovNo, safeOff, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			netpolicytest.Mark(t)
			got := netpolicy.BindGuardDecision(c.auth, c.host, c.mode, c.plat, c.ov, c.ub)
			assert.Equal(t, c.want, got)
		})
	}
}

// --- BridgeRefusalDecision --------------------------------------------------

func TestBridgeRefusalDecision(t *testing.T) {
	netpolicytest.Mark(t)
	// bridge default without opt-in ⇒ refuse.
	assert.True(t, netpolicy.BridgeRefusalDecision(runtime.NetworkModeBridge, false))
	// bridge default with explicit opt-in ⇒ allowed.
	assert.False(t, netpolicy.BridgeRefusalDecision(runtime.NetworkModeBridge, true))
	// internal/none never trip the bridge refusal.
	assert.False(t, netpolicy.BridgeRefusalDecision(runtime.NetworkModeInternal, false))
	assert.False(t, netpolicy.BridgeRefusalDecision(runtime.NetworkModeNone, false))
}

// --- Policy.EffectiveDefault ------------------------------------------------

func TestEffectiveDefault(t *testing.T) {
	netpolicytest.Mark(t)
	assert.Equal(t, runtime.NetworkModeInternal, netpolicy.Policy{}.EffectiveDefault())
	assert.Equal(t, runtime.NetworkModeInternal,
		netpolicy.Policy{DefaultMode: ""}.EffectiveDefault())
	assert.Equal(t, runtime.NetworkModeBridge,
		netpolicy.Policy{DefaultMode: runtime.NetworkModeBridge}.EffectiveDefault())
	assert.Equal(t, runtime.NetworkModeNone,
		netpolicy.Policy{DefaultMode: runtime.NetworkModeNone}.EffectiveDefault())
}

// --- ResolveAndValidate (the per-sandbox ceiling + ports + label strip) -----

func cfg(mode runtime.NetworkMode, ports ...runtime.PortMapping) *runtime.SandboxConfig {
	return &runtime.SandboxConfig{NetworkMode: mode, Ports: ports}
}

func TestResolveAndValidate_Ceiling(t *testing.T) {
	netpolicytest.Mark(t)
	p := netpolicy.Policy{DefaultMode: runtime.NetworkModeInternal}

	// "" inherits the global default.
	c := cfg("")
	require.NoError(t, p.ResolveAndValidate(c))
	assert.Equal(t, runtime.NetworkModeInternal, c.NetworkMode)

	// "none" is allowed and increases isolation.
	c = cfg(runtime.NetworkModeNone)
	require.NoError(t, p.ResolveAndValidate(c))
	assert.Equal(t, runtime.NetworkModeNone, c.NetworkMode)

	// Any other value — including one equal to the global default — is 400.
	for _, bad := range []runtime.NetworkMode{
		runtime.NetworkModeInternal, // value-equal-to-global is still rejected
		runtime.NetworkModeBridge,
		"None",   // case-sensitive: not "none"
		" none ", // whitespace not trimmed
		"null",
		"garbage",
	} {
		c = cfg(bad)
		err := p.ResolveAndValidate(c)
		var verr *netpolicy.ValidationError
		require.ErrorAs(t, err, &verr, "mode %q must be a ValidationError", bad)
		assert.Equal(t, "network_mode", verr.Field)
	}
}

func TestResolveAndValidate_Ports(t *testing.T) {
	netpolicytest.Mark(t)
	p := netpolicy.Policy{DefaultMode: runtime.NetworkModeBridge}

	// Protocol normalization: "", "tcp", "TCP" all canonicalize to "tcp".
	for _, proto := range []string{"", "tcp", "TCP", "Tcp"} {
		c := cfg("", runtime.PortMapping{SandboxPort: 8000, HostPort: 8000, Protocol: proto})
		require.NoError(t, p.ResolveAndValidate(c), "proto %q", proto)
		assert.Equal(t, "tcp", c.Ports[0].Protocol)
	}

	// udp is rejected with a ports.protocol ValidationError.
	c := cfg("", runtime.PortMapping{SandboxPort: 8000, HostPort: 8000, Protocol: "udp"})
	var verr *netpolicy.ValidationError
	require.ErrorAs(t, p.ResolveAndValidate(c), &verr)
	assert.Equal(t, "ports.protocol", verr.Field)

	// Both ends are range-checked.
	for _, bad := range []runtime.PortMapping{
		{SandboxPort: 0, HostPort: 8000},
		{SandboxPort: 70000, HostPort: 8000},
		{SandboxPort: 8000, HostPort: 0},
		{SandboxPort: 8000, HostPort: 99999},
	} {
		c := cfg("", bad)
		require.ErrorAs(t, p.ResolveAndValidate(c), &verr)
		assert.Contains(t, verr.Field, "ports.")
	}

	// none + ports ⇒ ports ValidationError.
	c = cfg(runtime.NetworkModeNone, runtime.PortMapping{SandboxPort: 8000, HostPort: 8000})
	require.ErrorAs(t, p.ResolveAndValidate(c), &verr)
	assert.Equal(t, "ports", verr.Field)
}

func TestResolveAndValidate_StripsDenLabels(t *testing.T) {
	netpolicytest.Mark(t)
	p := netpolicy.Policy{}
	c := &runtime.SandboxConfig{
		Labels: map[string]string{
			"den.id":      "spoofed",
			"den.managed": "true",
			"user.app":    "keepme",
		},
	}
	require.NoError(t, p.ResolveAndValidate(c))
	_, hasID := c.Labels["den.id"]
	_, hasManaged := c.Labels["den.managed"]
	assert.False(t, hasID, "den.id must be stripped")
	assert.False(t, hasManaged, "den.managed must be stripped")
	assert.Equal(t, "keepme", c.Labels["user.app"], "non-den labels preserved")
}

// --- Committed-string contract ---------------------------------------------

func TestCommittedStringsStable(t *testing.T) {
	netpolicytest.Mark(t)
	// These substrings are part of the operator/security contract; the e2e
	// script and integration matrix grep for them. A change here is a
	// deliberate, test-breaking contract change.
	assert.Contains(t, netpolicy.MsgBindRefusal, "guino refuses to start")
	assert.Contains(t, netpolicy.MsgBindRefusal, "platform_override")
	assert.Contains(t, netpolicy.MsgBindRefusal, "allow_unsafe_bind")
	assert.Contains(t, netpolicy.MsgBridgeRefusal, "guino refuses to start")
	assert.Contains(t, netpolicy.MsgBridgeRefusal, "allow_unsafe_bridge")
	assert.Contains(t, netpolicy.MsgUnsafeBindEnabled, "SECURITY:")
	assert.Contains(t, netpolicy.MsgPlatformOverrideAttested, "SECURITY:")
	assert.Contains(t, netpolicy.MsgPlatformOverrideAttested, "VOID")
	assert.Contains(t, netpolicy.MsgInternalPortsInert, "inert")
}
