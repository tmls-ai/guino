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
| `resource.enable_auto_throttle` | `true` | Lower each sandbox's soft limit (`memory.high`) under pressure (Linux only) |

The quickstart sets a one-core quota and 512 MiB memory limit. Do not assume default resource sharing guarantees a particular number of active workloads or prevents host OOM events.

Pressure is classified as Normal, Warning, High, Critical and Emergency with hysteresis. Critical/Emergency pressure rejects new sandboxes with HTTP 503. Pressure throttling supplements explicit limits and does not reserve dedicated memory for each sandbox.

### Pressure throttling

At High pressure and above, Guino divides 80% of free host memory across the running sandboxes and writes that share to each container's cgroup v2 `memory.high`. `memory.high` is a soft limit: the kernel reclaims memory and slows the workload, but does not kill it. When pressure falls back to Normal or Warning, Guino sets `memory.high` back to `max`.

Throttling never changes a sandbox's hard limit (`memory`, which becomes `HostConfig.Memory` and cgroup `memory.max`):

- `memory.high` never goes above the memory the sandbox requested. A sandbox created with `memory: 5 GiB` keeps its 5 GiB cap.
- The configured floor (`resource.min_memory_floor`) never raises `memory.high` above that request.
- Removing the throttle resets only `memory.high`. The requested hard limit stays.

Throttling needs a writable cgroup v2 `memory.high`. Where it is not available, Guino logs one warning and skips throttling for the life of the process. Admission control (HTTP 503 at Critical pressure) still applies. This is the case:

- on macOS with Docker Desktop, where containers run in a VM that Guino cannot reach and the Docker API offers only the hard limit,
- on Linux with cgroup v1, rootless Docker, or Guino in a container without a writable `/sys/fs/cgroup`.

Earlier versions lowered the hard limit in these cases. That turned the throttle into an OOM kill of a workload that was inside its own request.

Read `GET /api/v1/resources` for host pressure and resource status. Per-sandbox metrics are available through the API; CLI `guino stats` currently reports sandbox counts.
