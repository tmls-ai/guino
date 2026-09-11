package docker

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/runtime"
)

// --- buildContainerCreateSpec: the v9 none predicate + spoof resistance -----

func TestBuildContainerCreateSpec_NonePredicate(t *testing.T) {
	// Ports are deliberately set to prove they are dropped in none mode.
	cfg := runtime.SandboxConfig{
		Image: "x:latest",
		Ports: []runtime.PortMapping{{SandboxPort: 8080, HostPort: 8080, Protocol: "tcp"}},
	}
	cc, hc, nc, err := buildContainerCreateSpec("sb-1", cfg, "den-net", runtime.NetworkModeNone)
	require.NoError(t, err)

	// NetworkMode == "none"
	assert.Equal(t, container.NetworkMode("none"), hc.NetworkMode)
	// EndpointsConfig empty AND the passed networkID NOT substituted.
	assert.Empty(t, nc.EndpointsConfig, "none must not attach any network endpoint")
	// PortBindings empty (separate assert).
	assert.Empty(t, hc.PortBindings, "none must not publish ports")
	// ExposedPorts empty (separate assert).
	assert.Empty(t, cc.ExposedPorts, "none must not expose ports")
}

func TestBuildContainerCreateSpec_BridgePublishesOnLoopback(t *testing.T) {
	cfg := runtime.SandboxConfig{
		Image: "x:latest",
		Ports: []runtime.PortMapping{{SandboxPort: 3000, HostPort: 49152, Protocol: "tcp"}},
	}
	cc, hc, nc, err := buildContainerCreateSpec("sb-2", cfg, "den-net", runtime.NetworkModeBridge)
	require.NoError(t, err)

	binding, ok := hc.PortBindings["3000/tcp"]
	require.True(t, ok, "3000/tcp must be bound")
	require.Len(t, binding, 1)
	assert.Equal(t, "127.0.0.1", binding[0].HostIP, "host binding must be loopback-only")
	assert.Equal(t, "49152", binding[0].HostPort)

	_, exposed := cc.ExposedPorts["3000/tcp"]
	assert.True(t, exposed, "3000/tcp must be exposed")

	// Non-none modes attach the managed network.
	_, attached := nc.EndpointsConfig["den-net"]
	assert.True(t, attached, "bridge/internal must attach the managed network")
}

func TestBuildContainerCreateSpec_DenLabelsNotCallerSpoofable(t *testing.T) {
	cfg := runtime.SandboxConfig{
		Image: "x:latest",
		Labels: map[string]string{
			"den.id":      "spoofed",
			"den.created": "1970-01-01T00:00:00Z",
			"user.app":    "keepme",
		},
	}
	cc, _, _, err := buildContainerCreateSpec("real-id", cfg, "den-net", runtime.NetworkModeInternal)
	require.NoError(t, err)

	assert.Equal(t, "real-id", cc.Labels[labelID],
		"Guino-set den.id must win over a caller-spoofed value")
	assert.NotEqual(t, "1970-01-01T00:00:00Z", cc.Labels[labelCreated],
		"Guino-set den.created must win over a caller-spoofed value")
	assert.Equal(t, "keepme", cc.Labels["user.app"], "non-den labels preserved")
}

func TestBuildContainerCreateSpec_RejectsNonTCP(t *testing.T) {
	cfg := runtime.SandboxConfig{
		Image: "x:latest",
		Ports: []runtime.PortMapping{{SandboxPort: 53, HostPort: 53, Protocol: "udp"}},
	}
	_, _, _, err := buildContainerCreateSpec("sb-3", cfg, "den-net", runtime.NetworkModeBridge)
	require.Error(t, err, "udp must be rejected as defense-in-depth")
	assert.Contains(t, err.Error(), "protocol")
}

func TestBuildContainerCreateSpec_EmptyModeDefaultsInternal(t *testing.T) {
	cfg := runtime.SandboxConfig{Image: "x:latest"}
	_, hc, nc, err := buildContainerCreateSpec("sb-4", cfg, "den-net", "")
	require.NoError(t, err)
	assert.NotEqual(t, container.NetworkMode("none"), hc.NetworkMode)
	_, attached := nc.EndpointsConfig["den-net"]
	assert.True(t, attached, `"" mode must default to internal and attach den-net`)
}

// --- DaemonHost: runtime.docker_host is inert; DOCKER_HOST is authoritative --

func TestDaemonHost_DockerHostEnvIsAuthoritative(t *testing.T) {
	// Leg 2: DOCKER_HOST set ⇒ DaemonHost reflects it (construction is lazy;
	// no daemon contacted).
	t.Setenv("DOCKER_HOST", "tcp://example-daemon:2375")
	rt, err := New()
	require.NoError(t, err)
	assert.Equal(t, "tcp://example-daemon:2375", rt.DaemonHost())
}

func TestDaemonHost_ConfigDockerHostHasNoEffect(t *testing.T) {
	// Leg 1: DOCKER_HOST unset (empty ⇒ moby default). New() never reads the
	// dead runtime.docker_host, so DaemonHost is the FromEnv default and is
	// NOT the value an operator might have put in config.
	t.Setenv("DOCKER_HOST", "")
	rt, err := New()
	require.NoError(t, err)
	assert.NotEqual(t, "tcp://config-was-here:2375", rt.DaemonHost())
	assert.NotEmpty(t, rt.DaemonHost(), "a constructed moby client always has a host")
}
