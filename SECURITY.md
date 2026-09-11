# Security Policy

## Release status

Guino 0.1.0 is being prepared. Historical Den releases remain in Git history; this repository does not promise security maintenance for upstream release lines. Follow the migration status and release notes for the supported Guino version when published.

## Reporting a Vulnerability

Do not post exploit details, secrets or sensitive reproduction data in a public issue.

Use GitHub's **Report a vulnerability** action in this repository's Security tab when private vulnerability reporting is enabled. A verified private reporting channel is a release prerequisite. If that action is unavailable, ask a listed maintainer for a private reporting channel through a verified contact on their profile, without including vulnerability details in the initial request. This migration does not claim ownership of the former Den security email address.

Include the affected commit/version, deployment topology, reproduction steps, impact and any suggested fix in the private report. A response-time service level has not been established for Guino.

## Security Model

Guino targets local development and self-hosted environments with a trusted operator. Docker containers share a kernel: these controls do not constitute a universal boundary for hostile multi-tenant execution. Access to the Docker socket grants control over its host. API keys grant broad runtime access, not per-tenant authorization.

The runtime applies the following controls:

- **Capabilities**: `ALL` dropped; `NET_BIND_SERVICE`, `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE` and `FOWNER` added back
- **Read-only root filesystem by default**: tmpfs and explicit volumes are writable; a writable-root option changes this posture
- **PID limits**: Default 256 processes per container
- **CPU/memory**: Hard limits default to `0` (unlimited); configure explicit limits. Pressure monitoring is not an OOM guarantee
- **No new privileges**: `no-new-privileges` security option
- **Network posture**: `network_mode` ∈ `internal` (default) / `bridge` / `none`. **`none` disables external networking (container loopback remains)**; `internal` still reaches the bridge gateway, embedded DNS and host `0.0.0.0` services (see Known Limitations)
- **Port binding**: published only in `bridge` mode, bound to `127.0.0.1`; fixed at creation (no runtime add/remove — `501`)
- **Path validation**: Null byte and traversal protection on all file operations
- **Constant-time auth**: API key comparison resistant to timing attacks
- **SSRF protection**: S3 endpoints blocked from internal/private ranges by default; one operator-configured endpoint may be opted in (pinned, never metadata) — see §(4)

## Network Security Model & Platform Safety Matrix

This is the **primary security artifact** for the network feature. Read it before
running Guino with authentication disabled, a non-loopback bind, or `bridge` mode.

### (1) Platform safety matrix

The daemon topology determines where containers run, where published ports bind and which host services sandboxes can reach. A local-looking socket can be a proxy to another machine.

| Client/daemon topology | `none` | `internal` | `bridge` | Auth off + loopback, without override, in internal/bridge |
|---|---|---|---|---|
| Native Linux, co-resident daemon | Network disabled | Gateway/DNS/host services reachable | Same plus unfiltered egress; ports on daemon loopback | REFUSES |
| Rootless Linux daemon | Network disabled | Reachability depends on rootless network topology | Unfiltered egress; daemon-side port behavior | REFUSES |
| Remote daemon (`tcp://`/`ssh://`) | Network disabled | Daemon gateway/host services reachable | Unfiltered egress; ports on remote daemon host | REFUSES |
| Local unix socket proxied to another daemon | Network disabled | Looks local but reaches daemon-side services | Unfiltered egress; ports on actual daemon host | REFUSES |
| Docker Desktop, Colima/Lima or another VM daemon | Network disabled | VM gateway/DNS/services reachable | Egress from VM; host port forwarding depends on platform | REFUSES |

All modes share the container host kernel. "Network disabled" is not a kernel isolation or hostile tenant guarantee. Bridge startup also requires `runtime.allow_unsafe_bridge=true` in both `serve` and `mcp`.

With HTTP auth off, `none` permits startup; it does not prevent other users from reaching a non-loopback API bind. Choose a loopback bind for local use. With internal/bridge networking, a non-loopback unauthenticated bind refuses unless `allow_unsafe_bind` disables the guard.

**Operator attestation is an override, not a topology check.** Setting
`runtime.platform_override="linux-native-docker-co-resident"` substitutes the native-Linux classification for the bind decision, even when the probe reports another topology or fails. It permits an auth-off loopback bind. A false attestation on a VM/remote/rootless/proxied daemon can therefore bypass a refusal and expose the control plane. The software cannot prove this promise and does not mechanically reject those false claims.

Use that attestation only when the Guino process, Docker daemon, bridge gateway and kernel actually share a trusted native Linux host. It is unsupported on other topologies. Both it and `allow_unsafe_bind` are logged at ERROR on each HTTP startup and are risk-equivalent for the bind decision. Prefer authentication or `none` to an override.

### (2) The control plane is unauthenticated by default

`/api/v1/version`, `/api/v1/health`, and the embedded dashboard are reachable
**without an API key** even when auth is enabled (they are intentionally unauthenticated
liveness/UX surfaces). With auth disabled, the *entire* control plane — sandbox
create/exec/file I/O — is unauthenticated. The bind guard exists specifically so this
surface is not silently exposed to a `bridge`/`internal` sandbox or a LAN.

### (3) A shared kernel is not a tenant boundary

Guino containers share the host kernel. Capability drop, `no-new-privileges`,
read-only rootfs, seccomp and PID limits raise the bar but do not make this a
hard multi-tenant boundary. Hostile multi-tenant deployments require a separately assessed stronger isolation architecture; this migration does not implement or validate one.
A kernel-CVE pivot is possible from any mode including `none`.

### (4) SSRF protection scope

The SSRF allow/deny logic protects **Guino's own S3 client** (the endpoint Guino
itself connects to for import/export and S3 hooks). It is not a general egress
firewall for sandbox traffic; in `bridge` mode a sandbox has unrestricted
outbound network access.

**Threat model.** A sandbox — or a sandbox-influenced API request supplying a
per-sandbox S3 endpoint — must not be able to make Guino connect to internal
infrastructure (cloud metadata, link-local, loopback, RFC1918, CGNAT,
benchmark, unspecified) via a crafted endpoint or a DNS rebind. The single
home for this defense is `internal/security/ssrf`; it is stdlib-only and is
consumed identically by the storage transport and the API handlers, so the
early request reject and the actual dial cannot disagree.

**Default posture.** Every internal range is blocked. The configured endpoint
is resolved **once at client construction** and its **entire** resolved IP set
is *pinned*; the dialer never re-resolves, defeating the DNS-rebind TOCTOU
between validation and connection. `CheckRedirect` re-runs the same predicate
on every 3xx hop, so a region/host redirect cannot smuggle Guino onto an
internal box.

**Operator exemption — `s3.allow_internal_endpoint`.** Self-hosting
MinIO on `localhost` or the LAN is a legitimate, common deployment that the
default posture would make impossible. An operator may set
`s3.allow_internal_endpoint: true` to opt the **single configured
endpoint** back into loopback/RFC1918/CGNAT/benchmark reachability. The
trade-off is explicit: this re-permits exactly the configured host's pinned IP
set and nothing else. The API server logs it at startup (a `WARN`), the resolved
config is dumped with both keys masked, and:

- The exemption is **pinned to the construction-time IP set** — it is not a
  range allow, and a later DNS answer cannot widen it.
- **Cloud-metadata, link-local, multicast and unspecified addresses are NEVER
  reachable**, regardless of the flag or what the endpoint resolves to. A
  configured endpoint whose pinned set touches one of these is a **hard
  S3-client construction error**. Clients are created for operations/hooks,
  so server startup alone does not establish that endpoint resolution succeeds.
- A **per-sandbox endpoint override is refused** while the exemption is active
  (Gate B): the exemption is pinned to the one operator-configured endpoint,
  so a sandbox cannot redirect Guino at an arbitrary internal host. Bucket,
  region and credential overrides remain permitted — they change the object
  namespace, not the network host (an operator-side bucket-ACL question, not
  an SSRF).

**Trust model.** The configured endpoint is operator-controlled and trusted;
per-sandbox-supplied endpoints are untrusted. IDNA/punycode and
ambiguous-numeric (`127.1`, `0x7f.1`, `2130706433`) host forms are rejected
for the trusted endpoint rather than normalized — the endpoint must be a
canonical dotted-quad, bracketed IPv6, or ASCII DNS name.

**Out of scope.** The S3-FUSE mount path (`s3fs`, requires `SYS_ADMIN`,
disabled by default — see Known Limitations) connects from *inside* the
sandbox, not from Guino's client, and is **not** covered by this SSRF guard.
Pre-signed URLs handed to a sandbox are likewise opaque to Guino and out of
scope. These are documented limitations, not regressions.

### (5) Why `enable_icc=false` is sound here

Inter-container communication is disabled on the managed network. This is a real
control **only because** `NET_RAW` is dropped (no ARP/raw-socket sidestep) and the
Docker API floor is enforced at ≥ 1.42 (below it the typed `EnableIPv6 *bool` and
ICC options are silently ignored). Both conditions are checked at startup; if either
fails the network is not considered hardened.

### (6) `internal` does not contain a sandbox

In `internal` mode the sandbox still reaches the bridge gateway, the embedded DNS
resolver (`127.0.0.11`), and any host service bound to `0.0.0.0`. `internal` removes
NAT/egress to the *internet*, not reachability of the *host*. **Only
`network_mode=none` disables external networking (container loopback remains); kernel and shared-volume risks remain.**

### (7) Rejected: connectivity-on-by-default

We deliberately did **not** make `bridge` (full egress) the default. The default is
`internal`. This trades out-of-the-box internet for a smaller default attack surface;
operators who need sandbox egress must set the global mode to `bridge` and
explicitly enable `runtime.allow_unsafe_bridge`. A per-sandbox override accepts
only inheritance or `none`; it cannot enable egress. This decision
is recorded here so it is not silently reversed.

### (8) Out of scope for v1

Guino does not program host iptables/nftables and does not run as an egress-filtering
root. Per-`internal` egress filtering is a tracked follow-up, not in v1. Do not assume
`internal` will gain egress filtering without an explicit release note.

### (9) The feature token is a capability hint, not auth

`/api/v1/version` and `/api/v1/health` advertise a `features` list (e.g.
`network_mode`). SDKs use it lazily to fail fast on unsupported servers. It is a
**capability hint only** — it is not an authentication or authorization signal and
must never be treated as one.

### (10) The local-`unix://`-socket-proxied-to-remote residual is realistic, not rare

This is called out as its own point because it is the residual class operators most
often miss. A daemon reached through a **local-looking** unix socket that is actually
proxied to a remote/other-host daemon — `socat UNIX-LISTEN:/var/run/docker.sock
TCP:remote:2375`, `ssh -L .../docker.sock`, a docker-context-proxied socket, or a
bind-mounted rootless/sibling socket — is **common in CI, dev containers, and
remote-Docker setups**, not a corner case. On such a host:

- `127.0.0.1` host port bindings land on the **daemon** host, not the Guino host —
  published ports do not appear where the operator expects.
- The `platform_override` co-residency attestation is **false by construction**.
  It must not be used: the code trusts it and may permit an auth-off loopback
  bind despite the actual remote topology.
- Guino cannot reliably distinguish this topology from native-local at runtime, so
  "looks local" is treated as **insufficient**. The only real mitigations are
  `auth.enabled=true` or an effective `network_mode=none`; operators on this topology
  must **not** set `platform_override`.

### (11) `platform_override` risk equivalence

Setting `platform_override` is risk-equivalent to `allow_unsafe_bind`: both tell the
guard to permit a bind it would otherwise refuse. The difference is intent
documentation, not a stronger guarantee. It is **bind-guard-scoped** — it is not a
platform fact consulted anywhere else, and it relaxes no other control.

## Known Limitations

- Container isolation relies on Docker and the host kernel; stronger isolation requires a separate architecture and validation
- S3 FUSE mount requires `SYS_ADMIN` capability — disabled by default
- Authentication is disabled by default for local development convenience
- **`internal` is NOT a network boundary**: a sandbox still reaches the bridge gateway, the embedded DNS resolver (`127.0.0.11`) and any host service bound to `0.0.0.0`. `network_mode=none` disables external networking (container loopback remains); shared-kernel risks remain. Egress filtering for `internal` is a tracked follow-up, not in v1
- **Docker-out-of-Docker (DooD) port access is unsupported**: when guino's Docker client targets a remote or socket-proxied daemon (`tcp://`, `ssh://`, `socat`/`ssh -L`/docker-context/bind-mounted sibling socket), `127.0.0.1` port bindings land on the *daemon* host, not the guino host. The same proxied-socket topology also **voids** the `platform_override` co-residency attestation and re-exposes the unauthenticated control plane
- Dynamic port forwarding is not supported: `POST`/`DELETE /api/v1/sandboxes/{id}/ports` permanently return `501`; port mappings are fixed at sandbox creation
