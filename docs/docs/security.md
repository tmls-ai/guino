# Security

Guino is for local development and self-hosted deployments with a trusted operator. It executes code inside Docker containers, which share the host kernel. Container isolation is not a universal boundary for hostile tenants, even when networking is disabled.

## Runtime controls

- All Linux capabilities are dropped, then `NET_BIND_SERVICE`, `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE` and `FOWNER` are added back.
- Root filesystems are read-only by default; writable tmpfs, volumes and explicit writable-root options change that surface.
- `no-new-privileges` and a default PID limit of 256 are applied.
- CPU and memory hard limits default to zero (unlimited); set explicit limits for your workload.
- File operations validate paths. The S3 client rejects internal targets by default and pins endpoint addresses against DNS rebinding.

## Trust and networking

A process with access to the Docker socket controls the Docker host. Only trusted users should operate Guino or access its API. API keys are operator credentials, not per-tenant isolation or fine-grained authorization. Do not inject host secrets or mount sensitive host paths into sandboxes.

`internal` still permits reaching the bridge gateway, embedded DNS and host services. `bridge` has unfiltered outbound access and requires an explicit opt-in. `none` disables external networking (container loopback remains); it does not remove kernel, filesystem or resource exhaustion risks.

The API binds `0.0.0.0` and disables authentication by default, but startup guards refuse unsafe combinations. Use the loopback/`none` quickstart for a trusted local evaluation. Enable authentication and TLS before sharing an endpoint. Health, version and dashboard static content remain unauthenticated even with API authentication enabled.

## Further details

[Configuration](configuration.md) covers the network policy, bind guard, Docker platform classifier, S3 SSRF protections and dangerous overrides. [Self-hosting](self-hosting.md) explains state ownership and operations. The repository's [SECURITY.md](../../SECURITY.md) preserves the full platform matrix, threat model and vulnerability-reporting policy.
