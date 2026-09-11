//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tmls-ai/guino/internal/config"
	"github.com/tmls-ai/guino/internal/engine"
	"github.com/tmls-ai/guino/internal/runtime"
	"github.com/tmls-ai/guino/internal/runtime/docker"
	"github.com/tmls-ai/guino/internal/runtime/netpolicy"
	"github.com/tmls-ai/guino/internal/store"
)

// The network behavioral matrix. Every row is a real container on a real
// daemon with a UNIQUE managed network so rows never collide and each is
// force-removed in t.Cleanup. This is the executable companion to the
// SECURITY.md platform/safety matrix:
//
//	bridge   → egress (DNS+HTTPS) open  AND  host-published port reachable
//	internal → egress closed           AND  ports NOT published to the host
//	none     → no interface but lo     AND  ports request is a 400
//	ceiling  → per-sandbox may only tighten ("" / none); "internal" is a 400
//
// Image: busybox:latest. The official busybox image ships the FULL applet set
// (httpd / wget / nslookup); alpine:latest's stripped busybox has no httpd.
// Requires a live Docker daemon; built only under -tags integration.

const netTestImage = "busybox:latest"

func newNetEngine(t *testing.T, mode runtime.NetworkMode) *engine.Engine {
	t.Helper()

	netName := fmt.Sprintf("den-itest-%s-%d", mode, time.Now().UnixNano())
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	rt, err := docker.New(
		docker.WithNetworkID(netName),
		docker.WithNetworkMode(mode),
		docker.WithAllowUnsafeBridge(true), // test harness opt-in; not the product default
		docker.WithLogger(logger),
	)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, rt.Ping(ctx))
	require.NoError(t, rt.EnsureNetwork(ctx))

	dir := t.TempDir()
	st, err := store.NewBoltStore(dir + "/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })

	cfg := config.SandboxConfig{
		DefaultImage:    netTestImage,
		DefaultTimeout:  5 * time.Minute,
		MaxSandboxes:    10,
		DefaultCPU:      1_000_000_000,
		DefaultMemory:   256 * 1024 * 1024,
		DefaultPidLimit: 256,
		DefaultTmpfs: []config.TmpfsDefault{
			{Path: "/tmp", Size: "64m"},
			{Path: "/run", Size: "32m"},
		},
	}
	eng := engine.NewEngine(rt, st, cfg, config.S3Config{}, config.DefaultConfig().Resource,
		netpolicy.Policy{DefaultMode: mode}, logger)

	t.Cleanup(func() {
		eng.Shutdown(context.Background())
		// Force-remove the unique network even if a container lingered.
		_ = exec.Command("docker", "network", "rm", netName).Run()
	})
	return eng
}

func execIn(t *testing.T, eng *engine.Engine, id string, args ...string) (*runtime.ExecResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return eng.Exec(ctx, id, runtime.ExecOpts{Cmd: args})
}

// --- bridge: egress is open (DNS + HTTPS) -----------------------------------

func TestIntegration_Network_BridgeEgressOpen(t *testing.T) {
	eng := newNetEngine(t, runtime.NetworkModeBridge)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Image: netTestImage})
	require.NoError(t, err)

	dns, err := execIn(t, eng, sb.ID, "nslookup", "example.com")
	require.NoError(t, err)
	assert.Equal(t, 0, dns.ExitCode, "bridge: DNS must resolve\n%s", dns.Stdout+dns.Stderr)

	https, err := execIn(t, eng, sb.ID, "wget", "-q", "-T", "10", "-O", "/dev/null", "https://example.com")
	require.NoError(t, err)
	assert.Equal(t, 0, https.ExitCode, "bridge: outbound HTTPS must succeed")
}

// --- bridge: host-published port is reachable on loopback (the headline) ----

func TestIntegration_Network_BridgePublishedPortReachable(t *testing.T) {
	eng := newNetEngine(t, runtime.NetworkModeBridge)
	const hostPort = 49230
	const body = "den-e2e-ok"

	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{
		Image: netTestImage,
		Ports: []runtime.PortMapping{{SandboxPort: 8080, HostPort: hostPort, Protocol: "tcp"}},
	})
	require.NoError(t, err)

	// Port 8080 (unprivileged) by convention — no privileged-port dependency in
	// the proof. busybox httpd daemonizes; serve a known body from /tmp/www.
	start, err := execIn(t, eng, sb.ID, "sh", "-c",
		fmt.Sprintf("mkdir -p /tmp/www && printf %q > /tmp/www/index.html && httpd -p 8080 -h /tmp/www", body))
	require.NoError(t, err)
	require.Equal(t, 0, start.ExitCode, "httpd failed to start: %s", start.Stderr)

	// From the HOST (this test process) hit 127.0.0.1:hostPort.
	var got string
	var code int
	require.Eventually(t, func() bool {
		resp, e := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", hostPort))
		if e != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got, code = string(b), resp.StatusCode
		return true
	}, 15*time.Second, 300*time.Millisecond, "published port never became reachable from host loopback")

	assert.Equal(t, http.StatusOK, code, "bridge: published port must answer 200 on host loopback")
	assert.Contains(t, got, body, "bridge: served body must round-trip host→sandbox")
}

// --- internal: egress is closed --------------------------------------------

func TestIntegration_Network_InternalEgressClosed(t *testing.T) {
	eng := newNetEngine(t, runtime.NetworkModeInternal)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Image: netTestImage})
	require.NoError(t, err)

	dns, err := execIn(t, eng, sb.ID, "nslookup", "example.com")
	require.NoError(t, err)
	assert.NotEqual(t, 0, dns.ExitCode, "internal: DNS resolution must fail (no egress)")

	https, err := execIn(t, eng, sb.ID, "wget", "-q", "-T", "5", "-O", "/dev/null", "https://example.com")
	require.NoError(t, err)
	assert.NotEqual(t, 0, https.ExitCode, "internal: outbound HTTPS must fail")
}

// --- internal: ports are NOT published to the host --------------------------

func TestIntegration_Network_InternalNoHostPublish(t *testing.T) {
	eng := newNetEngine(t, runtime.NetworkModeInternal)
	const hostPort = 49231

	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{
		Image: netTestImage,
		Ports: []runtime.PortMapping{{SandboxPort: 8080, HostPort: hostPort, Protocol: "tcp"}},
	})
	require.NoError(t, err)
	start, err := execIn(t, eng, sb.ID, "sh", "-c", "mkdir -p /tmp/www && echo hi > /tmp/www/index.html && httpd -p 8080 -h /tmp/www")
	require.NoError(t, err)
	require.Equal(t, 0, start.ExitCode, "httpd must start before its host port can be tested: %s", start.Stderr)

	// internal must not bind a host port: the connection must be refused for
	// the full window (not merely "slow to come up").
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		c := &http.Client{Timeout: 800 * time.Millisecond}
		resp, e := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", hostPort))
		if e == nil {
			resp.Body.Close()
			t.Fatalf("internal: host port %d unexpectedly reachable — publish must be inert", hostPort)
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// --- none: no managed-network veth, and egress closed -----------------------

func TestIntegration_Network_NoneIsolated(t *testing.T) {
	eng := newNetEngine(t, runtime.NetworkModeNone)
	sb, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{Image: netTestImage})
	require.NoError(t, err)

	// The none-mode contract is "no managed-network veth attached", proven by
	// the ABSENCE of eth0 (the veth Docker injects for connected modes) while
	// lo is present. We deliberately do NOT assert the listing equals exactly
	// "lo": the Docker-host VM kernel surfaces phantom tunnel pseudo-devices
	// (gre0, sit0, ip_vti0, erspan0, ip6tnl0, bonding_masters, …) in
	// /sys/class/net from loaded kernel modules — they carry no address or
	// route and are not reachability. Zero reachability is proven behaviorally
	// by the nslookup failure below.
	ifaces, err := execIn(t, eng, sb.ID, "ls", "/sys/class/net")
	require.NoError(t, err)
	require.Equal(t, 0, ifaces.ExitCode)
	names := strings.Fields(ifaces.Stdout)
	assert.Contains(t, names, "lo", "none: loopback must exist")
	assert.NotContains(t, names, "eth0",
		"none: no managed-network veth (eth0) may be attached\n%s", ifaces.Stdout)

	dns, err := execIn(t, eng, sb.ID, "nslookup", "example.com")
	require.NoError(t, err)
	assert.NotEqual(t, 0, dns.ExitCode, "none: DNS must fail (no network at all)")
}

// --- none + ports: a 400, never a silently-inert publish --------------------

func TestIntegration_Network_NonePlusPortsRejected(t *testing.T) {
	eng := newNetEngine(t, runtime.NetworkModeNone)
	_, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{
		Image: netTestImage,
		Ports: []runtime.PortMapping{{SandboxPort: 8080, HostPort: 49232, Protocol: "tcp"}},
	})
	require.Error(t, err, "none cannot publish ports — must be a validation error, not a no-op")

	var verr *netpolicy.ValidationError
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, "ports", verr.Field)
}

// --- per-sandbox ceiling: may only tighten ----------------------------------

func TestIntegration_Network_PerSandboxCeiling(t *testing.T) {
	// Global default is bridge (egress open).
	eng := newNetEngine(t, runtime.NetworkModeBridge)

	// (a) per-sandbox "none" tightens → effective none → egress closed.
	sbNone, err := eng.CreateSandbox(context.Background(), runtime.SandboxConfig{
		Image:       netTestImage,
		NetworkMode: runtime.NetworkModeNone,
	})
	require.NoError(t, err)
	dns, err := execIn(t, eng, sbNone.ID, "nslookup", "example.com")
	require.NoError(t, err)
	assert.NotEqual(t, 0, dns.ExitCode,
		"per-sandbox none must override a bridge default and close egress")

	// (b) per-sandbox "internal" is NOT "" or "none" → rejected (a request may
	// only increase isolation, and only via "" or "none").
	_, err = eng.CreateSandbox(context.Background(), runtime.SandboxConfig{
		Image:       netTestImage,
		NetworkMode: runtime.NetworkModeInternal,
	})
	require.Error(t, err, "per-sandbox network_mode=internal must be rejected")
	var verr *netpolicy.ValidationError
	require.ErrorAs(t, err, &verr)
	assert.Equal(t, "network_mode", verr.Field)
}
