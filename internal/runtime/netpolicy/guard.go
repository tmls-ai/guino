package netpolicy

import (
	"net"
	"strings"

	"github.com/docker/docker/api/types/system"

	"github.com/tmls-ai/guino/internal/runtime"
)

// ClassifyHost maps the configured server.host to a HostClass.
//
// loopback iff the host is a literal IP in 127.0.0.0/8 or ::1. Empty,
// 0.0.0.0, ::, any hostname (including "localhost"), any non-loopback IP, and
// anything unparseable all classify as non-loopback (fail-closed): a hostname
// can resolve anywhere and "localhost" is not guaranteed to be loopback-only.
func ClassifyHost(host string) HostClass {
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return HostLoopback
	}
	return HostNonLoopback
}

// ClassifyPlatform is a POSITIVE allowlist. It returns PlatformLinuxNativeDocker
// iff ALL of the following hold; any failed/missing/unparseable clause yields
// PlatformUnknown (fail-closed). It never detects-the-bad-and-passes-the-rest.
//
//	(a) the docker info call succeeded — enforced at the call site: a failed
//	    SystemInfo(ctx) MUST be mapped to PlatformUnknown WITHOUT calling this
//	    function (clause kept off the pure signature so it stays total over
//	    an injected system.Info in unit tests).
//	(b) clientGOOS == "linux"  — the Guino process's own runtime.GOOS; closes
//	    the macOS/Windows-host-via-unix://-socket-to-Linux-VM hole
//	(c) info.OSType == "linux"
//	(d) info.OperatingSystem does NOT contain "Docker Desktop"
//	(e) info.KernelVersion contains neither "linuxkit" nor
//	    "microsoft-standard-WSL2"
//	(f) DecodeSecurityOptions(info.SecurityOptions) succeeds and yields no
//	    name=rootless (a present-but-empty slice satisfies this)
//	(g) effectiveDockerHost is local: empty (defensive-only; never produced
//	    by a real client) or a unix:// scheme. tcp://, ssh://, npipe:// are
//	    remote ⇒ not co-resident.
//
// effectiveDockerHost MUST be client.DaemonHost() (negotiated, post-resolution),
// never raw os.Getenv("DOCKER_HOST") nor the dead runtime.docker_host.
func ClassifyPlatform(info system.Info, effectiveDockerHost, clientGOOS string) RuntimePlatform {
	// (b)
	if clientGOOS != "linux" {
		return PlatformUnknown
	}
	// (c)
	if info.OSType != "linux" {
		return PlatformUnknown
	}
	// (d)
	if strings.Contains(info.OperatingSystem, "Docker Desktop") {
		return PlatformUnknown
	}
	// (e)
	if strings.Contains(info.KernelVersion, "linuxkit") ||
		strings.Contains(info.KernelVersion, "microsoft-standard-WSL2") {
		return PlatformUnknown
	}
	// (f)
	opts, err := system.DecodeSecurityOptions(info.SecurityOptions)
	if err != nil {
		return PlatformUnknown
	}
	for _, o := range opts {
		if o.Name == "rootless" {
			return PlatformUnknown
		}
	}
	// (g) — scheme matched as a scheme prefix, not a substring.
	if !isLocalDockerHost(effectiveDockerHost) {
		return PlatformUnknown
	}
	return PlatformLinuxNativeDocker
}

// isLocalDockerHost reports whether the (post-negotiation) daemon host points
// at a local unix socket. Empty is treated as local only defensively: a
// successfully constructed moby client never reports an empty DaemonHost().
func isLocalDockerHost(daemonHost string) bool {
	if daemonHost == "" {
		return true
	}
	return strings.HasPrefix(daemonHost, "unix://")
}

// BindGuardDecision is the pure safety decision for the HTTP control plane.
// It returns whether starting is SAFE. The caller (cmd/guino) is responsible for
// making it a no-op when there is no HTTP listener (MCP-only mode).
//
// Safe iff ANY of:
//   - allowUnsafeBind == true (explicit opt-in to unsafe), OR
//   - authEnabled == true (control plane is authenticated), OR
//   - effectiveMode == none (no network ⇒ no path to the control plane), OR
//   - hostClass == loopback AND platform == linux-native-docker AND
//     platformOverrideAttested == true.
//
// The loopback branch is REFUSE-by-default: a genuinely native-Linux loopback
// auth-off host with platform_override UNSET ⇒ platformOverrideAttested==false
// ⇒ NOT safe. There is no permit-with-WARN path.
func BindGuardDecision(
	authEnabled bool,
	hostClass HostClass,
	effectiveMode runtime.NetworkMode,
	platform RuntimePlatform,
	platformOverrideAttested bool,
	allowUnsafeBind bool,
) (safe bool) {
	if allowUnsafeBind {
		return true
	}
	if authEnabled {
		return true
	}
	if effectiveMode == runtime.NetworkModeNone {
		return true
	}
	if hostClass == HostLoopback &&
		platform == PlatformLinuxNativeDocker &&
		platformOverrideAttested {
		return true
	}
	return false
}

// BridgeRefusalDecision reports whether Guino must refuse to start because the
// effective global default mode is bridge without an explicit unsafe opt-in.
// This runs in BOTH HTTP and MCP mode (a bridge sandbox has unfiltered egress
// regardless of whether Guino exposes an HTTP listener).
func BridgeRefusalDecision(globalDefaultMode runtime.NetworkMode, allowUnsafeBridge bool) (refuse bool) {
	return globalDefaultMode == runtime.NetworkModeBridge && !allowUnsafeBridge
}
