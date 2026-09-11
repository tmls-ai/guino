// Package netpolicy holds Guino's network-policy decision logic: the per-sandbox
// create-time validator and the pure host/platform classifiers + bind/bridge
// guard decision functions.
//
// Everything here is deliberately pure and dependency-light (stdlib + the moby
// system.Info type + the runtime mode enum) so it can be unit-tested standalone
// with no socket and no live Docker daemon. The wiring that turns these
// decisions into a fatal startup refusal lives in cmd/guino.
package netpolicy

// Committed user-facing strings. These are part of the operator/security
// contract: guard tests assert their presence verbatim, so changing them is a
// behavior change, not a cosmetic edit. Keep them in this single block.
const (
	// MsgBindRefusal is emitted (and the process exits non-zero) when the
	// bind guard refuses to start. The remediation list is ordered
	// safest-first; allow_unsafe_bind is explicitly last-resort.
	MsgBindRefusal = "guino refuses to start: the HTTP control plane would be reachable from sandboxes without authentication on a host that is not machine-detectably safe. " +
		"Remediations (safest first): set auth.enabled=true with api_keys; " +
		"or set runtime.default_network_mode=none; " +
		"or, only if the Docker daemon, the bridge gateway and this Guino process are the same native-Linux host, " +
		"attest co-residency with runtime.platform_override=\"linux-native-docker-co-resident\"; " +
		"or, as a dangerous last resort, set runtime.allow_unsafe_bind=true"

	// MsgUnsafeBindEnabled is logged at ERROR on every start when
	// allow_unsafe_bind is set.
	MsgUnsafeBindEnabled = "SECURITY: runtime.allow_unsafe_bind=true — the unauthenticated Guino control plane is exposed to sandboxes; " +
		"a sandbox can reach the Guino API via the network gateway and escalate to the host. This bypasses the platform classifier entirely."

	// MsgPlatformOverrideAttested is logged at ERROR on every start when
	// runtime.platform_override attests co-residency. It is risk-equivalent
	// to bypassing the classifier and must be as loud as MsgUnsafeBindEnabled.
	MsgPlatformOverrideAttested = "SECURITY: runtime.platform_override=\"linux-native-docker-co-resident\" — operator has attested the Docker socket is local and co-resident. " +
		"This is VOID if the local unix:// socket is itself proxied to a remote/VM daemon (socat/ssh -L/docker-context/bind-mounted sibling socket); " +
		"in that case the unauthenticated Guino control plane is exposed. Risk-equivalent to bypassing the platform classifier."

	// MsgBridgeRefusal is emitted (and the process exits non-zero) when the
	// bridge-refusal guard refuses to start.
	MsgBridgeRefusal = "guino refuses to start: runtime.default_network_mode=bridge gives every sandbox NAT'd, unfiltered egress " +
		"(reaches RFC1918, link-local metadata and any host service) with no egress filter in v1. " +
		"Set runtime.allow_unsafe_bridge=true to accept this, or use internal/none."

	// MsgInternalPortsInert is the create/startup warning that port mappings
	// requested for an internal-mode sandbox are inert.
	MsgInternalPortsInert = "port mappings are inert in network_mode=internal (no host publishing on an internal network); use bridge to publish ports"

	// PlatformOverrideCoResident is the single literal accepted for
	// runtime.platform_override. Any other non-empty value is a config error.
	PlatformOverrideCoResident = "linux-native-docker-co-resident"
)

// HostClass is the classification of the configured server bind host.
type HostClass string

const (
	HostLoopback    HostClass = "loopback"
	HostNonLoopback HostClass = "non-loopback"
)

// RuntimePlatform is the positive-allowlist classification of the Docker
// runtime the Guino process is talking to.
type RuntimePlatform string

const (
	// PlatformLinuxNativeDocker is returned only when every allowlist clause
	// holds. It is the sole value that can satisfy the bind guard's
	// loopback branch (and only then together with an explicit attestation).
	PlatformLinuxNativeDocker RuntimePlatform = "linux-native-docker"
	// PlatformUnknown is the fail-closed default: any failed/missing/
	// unparseable clause yields this.
	PlatformUnknown RuntimePlatform = "unknown"
)

// PlatformOverrideAttested reports whether the operator has explicitly attested
// socket-locality / co-residency via runtime.platform_override. This is the v9
// MANDATORY clause that unlocks the bind guard's loopback branch; absent it, a
// native-Linux loopback auth-off host REFUSES to start.
func PlatformOverrideAttested(platformOverride string) bool {
	return platformOverride == PlatformOverrideCoResident
}
