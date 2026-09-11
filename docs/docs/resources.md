# Resource Management

Guino tracks host memory pressure and can throttle containers as pressure rises. See [Configuration](configuration.md) for every setting and environment variable.

| Setting | Default | Meaning |
|---------|---------|---------|
| `sandbox.default_cpu` | `0` | No hard CPU quota; NanoCPUs when nonzero |
| `sandbox.default_memory` | `0` | No hard memory limit; bytes when nonzero |
| `sandbox.default_pid_limit` | `256` | Per-container process limit |
| `sandbox.max_sandboxes` | `100` | Maximum managed sandboxes |
| `resource.overcommit_ratio` | `10.0` | Scheduling overcommit ratio |
| `resource.monitor_interval` | `5s` | Host memory sampling interval |
| `resource.enable_auto_throttle` | `true` | Apply throttling in response to pressure |

The quickstart sets a one-core quota and 512 MiB memory limit. Do not assume default resource sharing guarantees a particular number of active workloads or prevents host OOM events.

Pressure is classified as Normal, Warning, High, Critical and Emergency with hysteresis. Critical/Emergency pressure rejects new sandboxes with HTTP 503. Linux uses cgroup v2 where available; macOS uses Docker API fallback behavior. Pressure throttling supplements explicit limits and does not reserve dedicated memory for each sandbox.

Read `GET /api/v1/resources` for host pressure and resource status. Per-sandbox metrics are available through the API; CLI `guino stats` currently reports sandbox counts.
