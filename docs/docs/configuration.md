# Configuration

Configuration merges in order: **defaults → YAML file → legacy `DEN_*` variables → `GUINO_*` variables**.

An explicit `--config` selects a file. Without it, `guino.yaml` in the working directory is preferred, with deprecated `den.yaml` fallback through Guino 0.1.x. New environment names take precedence even when explicitly empty. Legacy use emits a warning to stderr.

The YAML below is a configuration reference with example limits, not a ready-to-run deployment: `0.0.0.0`, auth off and `internal` networking are refused by the bind guard. Use the [quickstart](quick-start.md) for a working local setup. Runtime defaults are `guino/default:latest`, 100 sandboxes, and CPU/memory limits of 0 (unlimited). Build the default image locally or choose an available image.

Persisted names `den.db` and `den-net` identify the runtime's state and managed network. Preserve them when upgrading so existing resources remain discoverable.

## guino.yaml

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  allowed_origins:
    - "http://localhost:8080"
  rate_limit_rps: 10
  rate_limit_burst: 20
  tls:
    enabled: false
    cert_file: ""
    key_file: ""

runtime:
  backend: "docker"
  docker_host: ""                # INERT — use the DOCKER_HOST env var
  network_id: "den-net"
  default_network_mode: "internal"  # internal | bridge | none
  reconcile_network: false
  allow_unsafe_bridge: false     # required to start with bridge
  allow_unsafe_bind: false       # dangerous last-resort; disables bind guard
  platform_override: ""          # "" or "linux-native-docker-co-resident"

sandbox:
  default_image: "ubuntu:22.04"
  default_timeout: "30m"
  max_sandboxes: 50
  default_cpu: 1000000000        # NanoCPUs (1e9 = 1 core)
  default_memory: 536870912      # bytes (512MB)
  default_pid_limit: 256
  warm_pool_size: 0
  allow_volumes: true
  allow_shared_volumes: true
  allow_s3: true
  allow_s3_fuse: false
  allow_host_binds: false        # NEVER enable in production
  max_volumes_per_sandbox: 5
  default_tmpfs:
    - path: "/tmp"
      size: "256m"
    - path: "/home/sandbox"
      size: "512m"
    - path: "/run"
      size: "64m"
    - path: "/var/tmp"
      size: "128m"

s3:
  endpoint: ""
  region: "us-east-1"
  access_key: ""
  secret_key: ""
  allow_internal_endpoint: false  # opt-in; see "S3 endpoint SSRF guard" below

resource:
  overcommit_ratio: 10.0          # Memory overcommit multiplier
  pressure_threshold: 0.80        # Warning level (80%)
  critical_threshold: 0.90        # Critical level (90%)
  monitor_interval: "5s"          # Pressure sampling interval
  enable_auto_throttle: true      # Dynamic memory.high adjustment
  min_memory_floor: 33554432      # 32MB minimum per container

store:
  path: "den.db"

auth:
  enabled: false
  api_keys:
    - "your-secret-key-here"

log:
  level: "info"                  # debug, info, warn, error
  format: "text"                 # text, json
```

## Environment Variables

Prefix `GUINO_` with `__` as nesting separator:

| Config | Environment Variable |
|--------|---------------------|
| `server.port` | `GUINO_SERVER__PORT` |
| `server.host` | `GUINO_SERVER__HOST` |
| `sandbox.default_image` | `GUINO_SANDBOX__DEFAULT_IMAGE` |
| `sandbox.default_timeout` | `GUINO_SANDBOX__DEFAULT_TIMEOUT` |
| `sandbox.max_sandboxes` | `GUINO_SANDBOX__MAX_SANDBOXES` |
| `sandbox.default_memory` | `GUINO_SANDBOX__DEFAULT_MEMORY` |
| `sandbox.default_cpu` | `GUINO_SANDBOX__DEFAULT_CPU` |
| `runtime.default_network_mode` | `GUINO_RUNTIME__DEFAULT_NETWORK_MODE` |
| `runtime.reconcile_network` | `GUINO_RUNTIME__RECONCILE_NETWORK` |
| `runtime.allow_unsafe_bridge` | `GUINO_RUNTIME__ALLOW_UNSAFE_BRIDGE` |
| `runtime.allow_unsafe_bind` | `GUINO_RUNTIME__ALLOW_UNSAFE_BIND` |
| `runtime.platform_override` | `GUINO_RUNTIME__PLATFORM_OVERRIDE` |
| `auth.enabled` | `GUINO_AUTH__ENABLED` |
| `log.level` | `GUINO_LOG__LEVEL` |
| `s3.endpoint` | `GUINO_S3__ENDPOINT` |
| `s3.access_key` | `GUINO_S3__ACCESS_KEY` |
| `s3.secret_key` | `GUINO_S3__SECRET_KEY` |
| `s3.allow_internal_endpoint` | `GUINO_S3__ALLOW_INTERNAL_ENDPOINT` |

## Network Modes & The Bind Guard

### Modes

`runtime.default_network_mode` sets the global default posture for every
sandbox. A per-sandbox `network_mode` (HTTP `network_mode`, MCP `network_mode`)
may only be `""` (inherit the global default) or `"none"` — a per-sandbox value
may only **increase** isolation, never decrease it. Any other per-sandbox value
(including one equal to the global default) is an **HTTP 400** / MCP tool error.

| Mode | Docker network | Egress | Host port publishing | Network isolation? |
|------|----------------|--------|----------------------|------------------|
| `internal` *(default)* | `den-net`, `Internal:true` | ✗ | ✗ (port mappings accepted but inert, with a warning) | **No** |
| `bridge` | `den-net`, `Internal:false` | ✓ unfiltered | ✓ `127.0.0.1` | No |
| `none` | none | ✗ | ✗ (`ports` ⇒ 400) | **Only loopback remains; shared kernel remains** |

> **`internal` does NOT contain a sandbox.** It still reaches the bridge
> gateway, the embedded DNS resolver (`127.0.0.11`) and any host service bound
> to `0.0.0.0`. `none` disables external networking (container loopback remains) but does not establish a hostile multi-tenant boundary. Egress filtering
> for `internal` is a tracked follow-up, not in v1.

`bridge` **refuses to start** unless `runtime.allow_unsafe_bridge=true`: there
is no egress filter in v1, so every bridge sandbox gets NAT'd, unfiltered egress
to RFC1918, link-local metadata and any host service. This refusal runs in
**both** `serve` and `mcp` mode.

### The bind guard

When the unauthenticated HTTP control plane would be reachable from sandboxes
on a host without an accepted authenticated/network-free configuration or explicit override, `guino serve` **refuses to
start** (non-zero exit, committed remediation message). It is a no-op in `mcp`
stdio mode (no HTTP listener), but the bridge refusal above still applies there.

Starting is permitted iff **any** of:

- `auth.enabled=true` with `api_keys` set (the control plane is authenticated), **or**
- effective network mode is `none` (no path from a sandbox to the control plane), **or**
- the API is loopback-bound **and**
  `runtime.platform_override="linux-native-docker-co-resident"` is set. This
  operator attestation substitutes the native-Linux classification for the
  bind decision, even if the probe failed or reported another topology, **or**
- `runtime.allow_unsafe_bind=true` (dangerous last-resort opt-in).

The loopback branch is **refuse-by-default**: a genuinely native-Linux,
loopback-bound, auth-off host with `platform_override` **unset** still refuses.
This is deliberate — co-residency of the Docker socket, the bridge gateway and
the guino process is **not machine-verifiable**, so it must be operator-attested.

`platform_override` accepts **exactly** `""` or the single literal
`"linux-native-docker-co-resident"` (case-sensitive; any other value is a fatal
config error). It is an **unsupported, false attestation** if the local `unix://` socket is itself proxied to
a remote/VM daemon (`socat` / `ssh -L` / docker-context / bind-mounted sibling
socket) — a realistic, not-rare class — in which case the override can permit an unsafe auth-off loopback bind. The code trusts the attestation; it does not prove co-residency or reject a false promise. Both `allow_unsafe_bind=true` and a set
`platform_override` are logged at **ERROR every start**; they are
risk-equivalent to bypassing the platform classifier.

### Platform classifier

The host is classified `linux-native-docker` only if **every** clause holds
(positive allowlist; any failure ⇒ `unknown` ⇒ fail-closed):

- the guino process's own `runtime.GOOS == "linux"` (closes the
  macOS/Windows-host-via-`unix://`-socket-to-Linux-VM hole),
- `docker info` succeeded and `OSType == "linux"`,
- `OperatingSystem` does not contain `Docker Desktop`,
- `KernelVersion` contains neither `linuxkit` nor `microsoft-standard-WSL2`,
- security options decode and contain no `name=rootless`,
- the negotiated Docker daemon host is a local `unix://` socket (not `tcp://`,
  `ssh://`, `npipe://`).

So loopback alone is **insufficient** on Docker Desktop, rootless, a remote
daemon, or a non-Linux guino host — those refuse unless `auth`/`none`/the
explicit opt-ins are used.

## Resource Management

Controls host memory pressure monitoring and dynamic container throttling.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `resource.overcommit_ratio` | float | `10.0` | Memory overcommit multiplier. Higher = more sandboxes per server, but higher pressure risk |
| `resource.pressure_threshold` | float | `0.80` | Memory usage ratio that triggers Warning level |
| `resource.critical_threshold` | float | `0.90` | Memory usage ratio that triggers Critical level (blocks new sandboxes) |
| `resource.monitor_interval` | duration | `5s` | How often to sample host memory pressure |
| `resource.enable_auto_throttle` | bool | `true` | Automatically apply cgroup v2 `memory.high` limits when pressure rises |
| `resource.min_memory_floor` | int | `33554432` (32MB) | Minimum memory per container, even under maximum pressure |

### Environment Variables

| Config | Environment Variable |
|--------|---------------------|
| `resource.overcommit_ratio` | `GUINO_RESOURCE__OVERCOMMIT_RATIO` |
| `resource.pressure_threshold` | `GUINO_RESOURCE__PRESSURE_THRESHOLD` |
| `resource.critical_threshold` | `GUINO_RESOURCE__CRITICAL_THRESHOLD` |
| `resource.monitor_interval` | `GUINO_RESOURCE__MONITOR_INTERVAL` |
| `resource.enable_auto_throttle` | `GUINO_RESOURCE__ENABLE_AUTO_THROTTLE` |
| `resource.min_memory_floor` | `GUINO_RESOURCE__MIN_MEMORY_FLOOR` |

### Notes

- Pressure levels use hysteresis: 2 consecutive readings are required before the level changes, preventing flapping
- On Linux, throttling uses direct cgroup v2 `memory.high` writes (sub-ms). On macOS, it falls back to Docker API
- When `enable_auto_throttle` is false, pressure is still monitored and reported via `GET /api/v1/resources`, but no limits are applied

## Validation

Server startup validates:

- `server.port` must be 1-65535
- `sandbox.max_sandboxes` must be positive
- `sandbox.default_memory` may be `0` (unlimited); a positive value must be ≥ 4MB
- `sandbox.default_timeout` must be valid Go duration
- `s3.allow_internal_endpoint=true` requires a non-empty, parseable
  `s3.endpoint`. Endpoint DNS resolution and pinned-address validation happen
  when an S3 client is constructed for an operation or hook, not necessarily at
  server startup. Resolved cloud-metadata / link-local / multicast / unspecified
  addresses cause client construction to fail regardless of the flag

## S3 endpoint SSRF guard

Guino's S3 client (import/export and S3 hooks) is protected by an SSRF guard so a
sandbox — or a sandbox-influenced per-request endpoint — cannot make Guino connect
to internal infrastructure.

- **Default (`s3.allow_internal_endpoint: false`).** Every internal range —
  loopback, RFC1918, CGNAT, link-local, multicast, cloud-metadata,
  benchmark, unspecified — is blocked. The configured endpoint is resolved
  **once at client construction** and its entire IP set is pinned; the dialer
  never re-resolves (DNS-rebind TOCTOU closed) and `CheckRedirect` re-validates
  every 3xx hop.
- **Self-hosted MinIO / LAN S3 (`s3.allow_internal_endpoint: true`).** Opts the
  **single configured endpoint** back into loopback / RFC1918 / CGNAT /
  benchmark reachability — and nothing else. The exemption is pinned to the
  construction-time IP set; cloud-metadata / link-local / multicast /
  unspecified stay permanently unreachable; and while it is active a
  **per-sandbox `endpoint` override is refused** (bucket / region / credential
  overrides still work). It is logged at `WARN` every start and the resolved
  config is dumped with both `access_key` and `secret_key` masked.

To use the trusted server endpoint from a sandbox, **omit `endpoint`** in the
per-sandbox S3 config — Guino falls back to the operator-configured endpoint
rather than accepting an untrusted one. See `SECURITY.md` §(4) for the full
threat model and trust boundary.

## Upgrading

See [Compatibility and upgrades](../migration.md) for supported configuration
aliases and persisted resource names. Check these settings when upgrading:

- **Bind guard.** `guino serve` refuses unsafe unauthenticated API exposure.
  Enable `auth.enabled` with non-empty `auth.api_keys`, or bind
  `server.host=127.0.0.1` and use `runtime.default_network_mode=none` for a
  trusted local setup. The
  `runtime.platform_override="linux-native-docker-co-resident"` attestation is
  supported only when the Docker socket, bridge gateway and Guino process are
  co-resident on native Linux. The code trusts this assertion and does not
  verify it; do not use it for proxied, remote or VM Docker. See the
  [security model](../../SECURITY.md) for the complete bind matrix.
- **Private S3 endpoints.** Access to S3/MinIO on `localhost` or the LAN is
  blocked by default. Configure the trusted endpoint and set
  `s3.allow_internal_endpoint: true` (environment variable
  `GUINO_S3__ALLOW_INTERNAL_ENDPOINT=true`) when this access is required.
  Per-sandbox endpoint overrides are refused while the exemption is active.
- **Diagnostic output.** The startup `"s3 config"` log line and
  `Config.String()` output mask both `access_key` and `secret_key`. Log
  processing must not rely on recovering credentials from these values.
