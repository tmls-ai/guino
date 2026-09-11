# Self-hosting

Guino runs entirely on infrastructure you control. Start with [Quick Start](quick-start.md) for a trusted local evaluation, then review the complete [Configuration](configuration.md) and [Security](security.md) guides.

## Configuration and authentication

Use a dedicated configuration file with an absolute `store.path`. Bind the API deliberately, enable `auth.enabled` with non-empty API keys before sharing it, and use TLS or a trusted TLS reverse proxy. API clients use `GUINO_API_KEY`; server keys are configured in `auth.api_keys`. Those settings serve different purposes.

The default bind is `0.0.0.0`, default auth is off and default networking is `internal`; the bind guard refuses unsafe exposure. Do not turn on `allow_unsafe_bind` merely to make startup succeed. `internal` still reaches host services; `none` is the network-free option. Bridge mode requires `allow_unsafe_bridge` because it permits unfiltered egress.

## Runtime ownership and backups

One Guino process must own a database and its Docker resources. Do not run Den, `serve`, or multiple MCP processes against those resources simultaneously. A different database alone does not isolate shared container ownership labels. Separate hosts/daemons are the straightforward way to isolate independent runtime instances.

Preserve the store file, Docker volumes and snapshot images when migrating. `den.db`, `den-net`, `den.*` labels, and existing snapshot/volume prefixes are intentional compatibility identifiers. Stop the runtime before making a consistent file backup; use Docker-aware backups for volume data and image snapshots. A database copy alone does not back up container data.

Graceful API server shutdown destroys running sandboxes. Tmpfs data is temporary. Abrupt shutdown may leave resources for startup reconciliation; the expiry loop needs a running engine. Keep important artifacts in backed-up volumes or export them explicitly.

## Docker topology

Guino uses the Docker client environment, including `DOCKER_HOST`; `runtime.docker_host` has no effect. A remote or socket-proxied daemon changes where containers and loopback-published ports live. A locally named socket does not prove that the daemon shares your host kernel. Do not set the co-residency override for Docker Desktop, a VM or a remote daemon.

## Capacity and monitoring

Set explicit CPU, memory, PID and sandbox-count limits for your machine. Monitor `GET /api/v1/resources`, container metrics, disk usage and logs. Memory pressure throttling does not guarantee that arbitrary overcommit is safe. See [Resource Management](resources.md).

S3 is optional. Internal S3/MinIO endpoints require the narrow `s3.allow_internal_endpoint` opt-in; it does not provide a general sandbox egress firewall. Protect keys, limit bucket access and verify backups separately.
