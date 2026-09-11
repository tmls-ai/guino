# REST API

S3/MinIO examples assume the operator has configured a trusted server-side `s3.endpoint`, credentials and `s3.allow_internal_endpoint: true` when that endpoint is private. Requests omit `endpoint` to inherit the configured endpoint: per-request overrides are refused while that exemption is active.

All endpoints are served under `/api/v1/`. Responses are JSON unless otherwise noted.

## Authentication

When auth is enabled, include the API key header:

```
X-API-Key: your-secret-key
```

Unauthenticated requests return `401 Unauthorized`.

## Health & Version

```
GET /api/v1/health        → {"status": "ok"}
GET /api/v1/version       → {"version": "0.0.6", "commit": "abc1234", "build_date": "..."}
```

## Sandboxes

### Create Sandbox

```
POST /api/v1/sandboxes
```

```json
{
  "image": "ubuntu:22.04",
  "env": {"MY_VAR": "value"},
  "workdir": "/home/sandbox",
  "timeout": 1800,
  "cpu": 1000000000,
  "memory": 536870912,
  "ports": [
    {"sandbox_port": 3000, "host_port": 0, "protocol": "tcp"}
  ],
  "storage": {
    "volumes": [
      {"name": "my-data", "mount_path": "/data", "read_only": false}
    ],
    "tmpfs": [
      {"path": "/tmp", "size": "128m", "options": "rw,noexec,nosuid"}
    ],
    "s3": {
      "bucket": "my-bucket",
      "prefix": "sandbox-data/",
      "access_key": "minioadmin",
      "secret_key": "minioadmin",
      "mode": "hooks",
      "sync_path": "/home/sandbox"
    }
  }
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `image` | string | `guino/default:latest` | Docker image |
| `env` | object | `{}` | Environment variables |
| `workdir` | string | `""` | Working directory |
| `timeout` | int | `1800` | Auto-expiry in seconds (30 min) |
| `cpu` | int | `0` | CPU in NanoCPUs (1e9 = 1 core) |
| `memory` | int | `0` | Memory in bytes; default 0 means unlimited |
| `ports` | array | `[]` | Port mappings (`host_port: 0` for auto-assign) |
| `storage` | object | `null` | Storage configuration (see below) |

**Storage fields:**

| Field | Type | Description |
|-------|------|-------------|
| `storage.volumes[].name` | string | Volume name (auto-prefixed with `den-`) |
| `storage.volumes[].mount_path` | string | Mount path inside container |
| `storage.volumes[].read_only` | bool | Read-only mount |
| `storage.tmpfs[].path` | string | Tmpfs mount path |
| `storage.tmpfs[].size` | string | Size (e.g. `256m`, `1g`) |
| `storage.tmpfs[].options` | string | Mount options (`rw,noexec,nosuid`) |
| `storage.s3.mode` | string | `hooks`, `fuse`, or `on_demand` |
| `storage.s3.bucket` | string | S3 bucket name |
| `storage.s3.prefix` | string | Key prefix for sync |
| `storage.s3.sync_path` | string | Local path for hooks mode |
| `storage.s3.mount_path` | string | Mount path for FUSE mode |

**S3 sync modes:**

- **`hooks`** — Auto-download on create, auto-upload on destroy
- **`fuse`** — Mount bucket as filesystem via s3fs (requires `allow_s3_fuse: true`)
- **`on_demand`** — Manual import/export via API endpoints

> **Per-request `endpoint` and the SSRF guard.** When the operator has set
> `s3.allow_internal_endpoint: true` (self-hosted MinIO/LAN), a per-sandbox or
> per-request `endpoint` is **refused** — the exemption is pinned to the single
> operator-configured endpoint. **Omit `endpoint`** to use the trusted server
> endpoint. `bucket`, `prefix`, `access_key` and `secret_key` overrides are
> still accepted (they change the object namespace, not the network host). With
> the exemption off (default), an `endpoint` resolving to any internal range is
> blocked. See `SECURITY.md` §(4).

Response `201`:

```json
{
  "id": "d6jcj6a9qf76oti2r2sg",
  "image": "ubuntu:22.04",
  "status": "running",
  "created_at": "2026-03-03T11:44:25.809Z",
  "expires_at": "2026-03-03T12:14:25.809Z"
}
```

### List / Get / Stop / Destroy

```
GET    /api/v1/sandboxes          → List all sandboxes
GET    /api/v1/sandboxes/{id}     → Get sandbox details
POST   /api/v1/sandboxes/{id}/stop → Stop sandbox
DELETE /api/v1/sandboxes/{id}     → Destroy sandbox (204)
```

## Command Execution

### Sync Exec

```
POST /api/v1/sandboxes/{id}/exec
```

```json
{
  "cmd": ["python3", "-c", "print('hello')"],
  "env": {"KEY": "value"},
  "workdir": "/tmp",
  "timeout": 30
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `cmd` | string[] | **required** | Command and arguments |
| `env` | object | `{}` | Environment variables |
| `workdir` | string | `""` | Working directory |
| `timeout` | int | `30` | Timeout in seconds (max 300) |

Response:

```json
{"exit_code": 0, "stdout": "hello\n", "stderr": ""}
```

### WebSocket Streaming

```
GET /api/v1/sandboxes/{id}/exec/stream
Upgrade: websocket
```

Send a JSON message after connecting:

```json
{"cmd": ["python3", "script.py"], "timeout": 60}
```

Server streams messages:

```json
{"type": "stdout", "data": "output line\n"}
{"type": "stderr", "data": "error line\n"}
{"type": "exit", "data": "0"}
```

Connection closes after the `exit` message.

## File Operations

All file paths via `path` query parameter. Must be absolute. Writable locations: `/tmp`, `/home/sandbox`, `/run`, `/var/tmp`.

```
GET    /api/v1/sandboxes/{id}/files?path=/tmp/hello.py          → Read file
PUT    /api/v1/sandboxes/{id}/files?path=/tmp/hello.py          → Write file (raw body)
GET    /api/v1/sandboxes/{id}/files/list?path=/tmp              → List directory
POST   /api/v1/sandboxes/{id}/files/mkdir?path=/tmp/mydir       → Create directory (204)
DELETE /api/v1/sandboxes/{id}/files?path=/tmp/hello.py           → Delete file/dir (204)
POST   /api/v1/sandboxes/{id}/files/upload?path=/tmp/data.bin   → Upload (multipart, max 100MB)
GET    /api/v1/sandboxes/{id}/files/download?path=/tmp/hello.py → Download (attachment)
```

List directory response:

```json
[
  {"name": "hello.py", "path": "/tmp/hello.py", "size": 21, "mode": "-rw-r--r--", "mod_time": "...", "is_dir": false},
  {"name": "data", "path": "/tmp/data", "size": 4096, "mode": "drwxr-xr-x", "mod_time": "...", "is_dir": true}
]
```

## S3 Import/Export

### Import from S3

```
POST /api/v1/sandboxes/{id}/files/s3-import
```

```json
{
  "bucket": "my-bucket",
  "key": "data/input.csv",
  "dest_path": "/home/sandbox/input.csv",
  "access_key": "minioadmin",
  "secret_key": "minioadmin"
}
```

Response: `{"success": true, "bytes_downloaded": 1048576, "path": "/home/sandbox/input.csv"}`

### Export to S3

```
POST /api/v1/sandboxes/{id}/files/s3-export
```

```json
{
  "source_path": "/home/sandbox/output.csv",
  "bucket": "my-bucket",
  "key": "results/output.csv"
}
```

Response: `{"success": true, "bytes_uploaded": 2048, "s3_key": "results/output.csv"}`

## Snapshots

Files in tmpfs (`/tmp`, `/home/sandbox`) are **not preserved** in snapshots. Write to non-tmpfs paths to persist across snapshots.

```
POST   /api/v1/sandboxes/{id}/snapshots              → Create snapshot
GET    /api/v1/sandboxes/{id}/snapshots               → List snapshots
POST   /api/v1/snapshots/{snapshotId}/restore         → Restore → new sandbox (201)
DELETE /api/v1/snapshots/{snapshotId}                  → Delete snapshot (204)
```

## Ports

```
GET    /api/v1/sandboxes/{id}/ports        → list (200)
POST   /api/v1/sandboxes/{id}/ports        → 501 Not Implemented
DELETE /api/v1/sandboxes/{id}/ports/{port} → 501 Not Implemented
```

```json
[{"sandbox_port": 3000, "host_port": 49152, "protocol": "tcp"}]
```

Port mappings are **fixed at sandbox creation** (the `ports` field of the create
request) and published **Docker-natively to `127.0.0.1` only, in `network_mode=bridge`
only** — inert in `internal` (the default), rejected in `none`. There is no userspace
forwarder and no runtime add/remove: `POST`/`DELETE /ports` permanently return `501`.
`GET /ports` returns the list configured at creation (empty unless ports were set).

## Statistics

```
GET /api/v1/sandboxes/{id}/stats    → Per-sandbox stats
GET /api/v1/stats                    → System-wide stats
```

Per-sandbox:

```json
{
  "cpu_percent": 2.5,
  "memory_usage": 15728640,
  "memory_limit": 536870912,
  "memory_percent": 2.93,
  "network_rx_bytes": 1024,
  "network_tx_bytes": 512,
  "disk_read_bytes": 4096,
  "disk_write_bytes": 2048,
  "pid_count": 3,
  "timestamp": "2026-03-03T12:00:00Z"
}
```

System-wide:

```json
{
  "total_sandboxes": 5,
  "running_sandboxes": 3,
  "stopped_sandboxes": 2,
  "total_snapshots": 2
}
```

## Resources

### Resource Status

```
GET /api/v1/resources
```

Returns host memory, sandbox count, and pressure information.

Response:

```json
{
  "host": {
    "memory_total": 8589934592,
    "memory_used": 5368709120,
    "memory_free": 3221225472,
    "cpu_cores": 4
  },
  "sandboxes": {
    "active": 42,
    "total": 50
  },
  "pressure": {
    "level": "normal",
    "score": 0.625,
    "can_create": true,
    "next_threshold": 0.80
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `host.memory_total` | int | Total host memory in bytes |
| `host.memory_used` | int | Used host memory in bytes |
| `host.memory_free` | int | Available host memory in bytes |
| `host.cpu_cores` | int | Number of CPU cores |
| `sandboxes.active` | int | Currently running sandboxes |
| `sandboxes.total` | int | Maximum allowed sandboxes |
| `pressure.level` | string | Current pressure level: `normal`, `warning`, `high`, `critical`, `emergency` |
| `pressure.score` | float | Memory usage ratio (0.0 - 1.0) |
| `pressure.can_create` | bool | Whether new sandboxes can be created at current pressure |
| `pressure.next_threshold` | float | Memory ratio that would trigger the next pressure level |

## Error Responses

```json
{"error": "description of what went wrong"}
```

| Status | Meaning |
|--------|---------|
| `400` | Bad request |
| `401` | Unauthorized |
| `404` | Not found |
| `408` | Exec timeout |
| `409` | Sandbox not running |
| `413` | Payload too large (1MB JSON, 100MB upload) |
| `429` | Rate limit exceeded |
| `500` | Internal error |
| `503` | Sandbox limit reached or memory pressure too high (Critical/Emergency) |

## Rate Limiting

Per-key token bucket (or per-IP when auth is disabled). Default: 10 req/s with burst of 20.
