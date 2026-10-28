---
title: API Reference
---

# API Reference

S3/MinIO examples assume the operator has configured a trusted server-side `s3.endpoint`, credentials and `s3.allow_internal_endpoint: true` when that endpoint is private. Requests omit `endpoint` to inherit the configured endpoint: per-request overrides are refused while that exemption is active.

All endpoints are served under `/api/v1/`. Responses are JSON unless otherwise noted.

## Authentication

When auth is enabled, include the API key header:

```
X-API-Key: your-secret-key
```

Unauthenticated requests return `401 Unauthorized`.

## Health & Version

### Health Check

```
GET /api/v1/health
```

Response `200 OK`:
```json
{"status": "ok"}
```

### Version

```
GET /api/v1/version
```

Response `200 OK`:
```json
{
  "version": "0.0.2",
  "commit": "abc1234",
  "build_date": "2026-03-03T00:00:00Z"
}
```

---

## Sandboxes

### Create Sandbox

```
POST /api/v1/sandboxes
Content-Type: application/json
```

Request body:
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
  ]
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `image` | string | `guino/default:latest` | Docker image to use |
| `env` | object | `{}` | Environment variables |
| `workdir` | string | `""` | Working directory |
| `timeout` | int | `1800` | Idle expiry in seconds (default 30 min). Exec, file, session and proxy activity pushes `expires_at` out again, and an open exec stream or proxied connection keeps the sandbox alive. This is not a hard lifetime cap: a sandbox in continuous use does not expire |
| `cpu` | int | `0` | CPU limit in NanoCPUs (1 core = 1e9) |
| `memory` | int | `0` | Memory limit in bytes; default 0 means unlimited |
| `ports` | array | `[]` | Port mappings (set `host_port: 0` for auto-assign) |
| `storage` | object | `null` | Storage configuration (see below) |
| `host_binds` | array | `[]` | Host directories to mount: `[{"host_path", "mount_path", "read_only"}]`. Refused with `422` unless the server sets `sandbox.allow_host_binds` and the resolved path is inside a configured `sandbox.host_bind_roots` entry |

Response `201 Created`:
```json
{
  "id": "d6jcj6a9qf76oti2r2sg",
  "image": "ubuntu:22.04",
  "status": "running",
  "created_at": "2026-03-03T11:44:25.809Z",
  "expires_at": "2026-03-03T12:14:25.809Z"
}
```

**Storage configuration** (`storage` field):

```json
{
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

| Storage Field | Type | Description |
|---------------|------|-------------|
| `storage.volumes` | array | Docker named volume mounts |
| `storage.volumes[].name` | string | Volume name (auto-prefixed with `den-`) |
| `storage.volumes[].mount_path` | string | Mount path inside container |
| `storage.volumes[].read_only` | bool | Mount as read-only (default `false`) |
| `storage.tmpfs` | array | Tmpfs mount overrides |
| `storage.tmpfs[].path` | string | Mount path |
| `storage.tmpfs[].size` | string | Size (e.g. `256m`, `1g`) |
| `storage.tmpfs[].options` | string | Mount options (e.g. `rw,noexec,nosuid`) |
| `storage.s3` | object | S3 sync configuration |
| `storage.s3.mode` | string | `hooks`, `fuse`, or `on_demand` |
| `storage.s3.bucket` | string | S3 bucket name |
| `storage.s3.prefix` | string | Key prefix for sync |
| `storage.s3.sync_path` | string | Local path for hooks mode (default `/home/sandbox`) |
| `storage.s3.mount_path` | string | Mount path for FUSE mode |
| `storage.s3.endpoint` | string | S3 endpoint (overrides server default) |
| `storage.s3.access_key` | string | Access key (overrides server default) |
| `storage.s3.secret_key` | string | Secret key (overrides server default) |

**S3 sync modes:**

- **`hooks`** — On sandbox create: download all objects under `prefix` to `sync_path`. On sandbox destroy: upload `sync_path` contents back to S3.
- **`fuse`** — Mount bucket as a filesystem at `mount_path` using s3fs. Requires `allow_s3_fuse: true` in server config.
- **`on_demand`** — No automatic sync. Use the S3 Import/Export API endpoints manually.

Error responses:
- `400` — Invalid request body
- `429` — Rate limit exceeded
- `503` — Maximum sandbox limit reached

### List Sandboxes

```
GET /api/v1/sandboxes
```

Response `200 OK`:
```json
[
  {
    "id": "d6jcj6a9qf76oti2r2sg",
    "image": "ubuntu:22.04",
    "status": "running",
    "created_at": "2026-03-03T11:44:25.809Z",
    "expires_at": "2026-03-03T12:14:25.809Z"
  }
]
```

### Get Sandbox

```
GET /api/v1/sandboxes/{id}
```

Response `200 OK`:
```json
{
  "id": "d6jcj6a9qf76oti2r2sg",
  "image": "ubuntu:22.04",
  "status": "running",
  "created_at": "2026-03-03T11:44:25.809Z",
  "expires_at": "2026-03-03T12:14:25.809Z"
}
```

Error: `404` — Sandbox not found

### Stop Sandbox

```
POST /api/v1/sandboxes/{id}/stop
```

Stops the container without removing it. The sandbox can be inspected but not used for exec or file operations.

Response `200 OK`:
```json
{"status": "stopped"}
```

Error: `404` — Sandbox not found

### Destroy Sandbox

```
DELETE /api/v1/sandboxes/{id}
```

Stops and removes the container and all associated state.

Response `204 No Content`

Error: `404` — Sandbox not found

---

## Command Execution

### Execute Command (Sync)

```
POST /api/v1/sandboxes/{id}/exec
Content-Type: application/json
```

Request body:
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
| `env` | object | `{}` | Additional environment variables |
| `workdir` | string | `""` | Working directory inside sandbox |
| `timeout` | int | `30` | Timeout in seconds (max 300) |

Response `200 OK`:
```json
{
  "exit_code": 0,
  "stdout": "hello\n",
  "stderr": ""
}
```

Error responses:
- `400` — Invalid request (empty cmd)
- `404` — Sandbox not found
- `409` — Sandbox is not running

### Execute Command (WebSocket Streaming)

```
GET /api/v1/sandboxes/{id}/exec/stream
Upgrade: websocket
```

Connect via WebSocket, then send a JSON message:

```json
{
  "cmd": ["python3", "script.py"],
  "env": {},
  "workdir": "/tmp",
  "timeout": 60
}
```

Server streams messages:

```json
{"type": "stdout", "data": "output line\n"}
{"type": "stderr", "data": "error line\n"}
{"type": "exit", "data": "0"}
```

On error:
```json
{"type": "error", "data": "execution failed"}
```

The connection closes after the `exit` message.

---

## File Operations

All file paths are specified via the `path` query parameter. Paths must be absolute.

Writable locations (with default security config):
- `/tmp`
- `/home/sandbox`
- `/run`
- `/var/tmp`

### Read File

```
GET /api/v1/sandboxes/{id}/files?path=/tmp/hello.py
```

Response `200 OK`: Raw file content (Content-Type based on extension)

Errors:
- `400` — Missing path parameter
- `404` — Sandbox or file not found
- `409` — Sandbox is not running

### Write File

```
PUT /api/v1/sandboxes/{id}/files?path=/tmp/hello.py
Content-Type: application/octet-stream

print("Hello World!")
```

Request body is the raw file content. Parent directories are created automatically.

Response `200 OK`:
```json
{"success": true}
```

Errors:
- `400` — Missing path parameter
- `404` — Sandbox not found
- `409` — Sandbox is not running

### List Directory

```
GET /api/v1/sandboxes/{id}/files/list?path=/tmp
```

Response `200 OK`:
```json
[
  {
    "name": "hello.py",
    "path": "/tmp/hello.py",
    "size": 21,
    "mode": "-rw-r--r--",
    "mod_time": "2026-03-03T12:00:00Z",
    "is_dir": false
  },
  {
    "name": "data",
    "path": "/tmp/data",
    "size": 4096,
    "mode": "drwxr-xr-x",
    "mod_time": "2026-03-03T11:55:00Z",
    "is_dir": true
  }
]
```

### Create Directory

```
POST /api/v1/sandboxes/{id}/files/mkdir?path=/tmp/mydir
```

Creates the directory and all parent directories.

Response `204 No Content`

### Delete File or Directory

```
DELETE /api/v1/sandboxes/{id}/files?path=/tmp/hello.py
```

Recursively removes the file or directory.

Response `204 No Content`

### Upload File (Multipart)

```
POST /api/v1/sandboxes/{id}/files/upload?path=/tmp/uploaded.bin
Content-Type: multipart/form-data

--boundary
Content-Disposition: form-data; name="file"; filename="data.bin"
Content-Type: application/octet-stream

<binary data>
--boundary--
```

Max upload size: 100MB

Response `204 No Content`

### Download File

```
GET /api/v1/sandboxes/{id}/files/download?path=/tmp/hello.py
```

Returns the file as a download with `Content-Disposition: attachment` header.

Response `200 OK`: Raw file content

---

## S3 Import/Export

Transfer files between sandboxes and S3-compatible storage (AWS S3, MinIO, etc.).

### Import from S3

```
POST /api/v1/sandboxes/{id}/files/s3-import
Content-Type: application/json
```

Request body:
```json
{
  "bucket": "my-bucket",
  "key": "data/input.csv",
  "dest_path": "/home/sandbox/input.csv",
  "access_key": "minioadmin",
  "secret_key": "minioadmin",
  "region": "us-east-1"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `bucket` | string | yes | S3 bucket name |
| `key` | string | yes | S3 object key |
| `dest_path` | string | yes | Destination path inside sandbox |
| `endpoint` | string | no | S3 endpoint (falls back to server default) |
| `access_key` | string | no | Access key (falls back to server default) |
| `secret_key` | string | no | Secret key (falls back to server default) |
| `region` | string | no | AWS region (falls back to server default) |

Response `200 OK`:
```json
{
  "success": true,
  "bytes_downloaded": 1048576,
  "path": "/home/sandbox/input.csv"
}
```

Errors:
- `400` — Invalid request, path traversal detected, object exceeds 100MB limit, or blocked endpoint
- `404` — Sandbox not found
- `409` — Sandbox is not running

### Export to S3

```
POST /api/v1/sandboxes/{id}/files/s3-export
Content-Type: application/json
```

Request body:
```json
{
  "source_path": "/home/sandbox/output.csv",
  "bucket": "my-bucket",
  "key": "results/output.csv",
  "access_key": "minioadmin",
  "secret_key": "minioadmin"
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source_path` | string | yes | File path inside sandbox |
| `bucket` | string | yes | S3 bucket name |
| `key` | string | yes | S3 object key |
| `endpoint` | string | no | S3 endpoint (falls back to server default) |
| `access_key` | string | no | Access key (falls back to server default) |
| `secret_key` | string | no | Secret key (falls back to server default) |
| `region` | string | no | AWS region (falls back to server default) |

Response `200 OK`:
```json
{
  "success": true,
  "bytes_uploaded": 2048,
  "s3_key": "results/output.csv"
}
```

Errors:
- `400` — Invalid request, path traversal detected, file exceeds 100MB limit, or blocked endpoint
- `404` — Sandbox not found
- `409` — Sandbox is not running

---

## Snapshots

Snapshots save the current state of a sandbox using `docker commit`. You can restore a snapshot to create a new sandbox in the same state.

> **Note:** Files stored in tmpfs mounts (`/tmp`, `/home/sandbox`, `/run`, `/var/tmp`) are **not preserved** in snapshots. Docker commit captures the container's writable layer, but tmpfs is stored in memory and not part of the layer. To persist files across snapshots, write them to a non-tmpfs path inside the container.

### Create Snapshot

```
POST /api/v1/sandboxes/{id}/snapshots
Content-Type: application/json
```

Request body:
```json
{
  "name": "after-setup"
}
```

Response `201 Created`:
```json
{
  "id": "snap_abc123",
  "sandbox_id": "d6jcj6a9qf76oti2r2sg",
  "name": "after-setup",
  "image_id": "sha256:...",
  "created_at": "2026-03-03T12:00:00Z",
  "size": 0
}
```

### List Snapshots

```
GET /api/v1/sandboxes/{id}/snapshots
```

Response `200 OK`:
```json
[
  {
    "id": "snap_abc123",
    "sandbox_id": "d6jcj6a9qf76oti2r2sg",
    "name": "after-setup",
    "image_id": "sha256:...",
    "created_at": "2026-03-03T12:00:00Z",
    "size": 0
  }
]
```

### Restore Snapshot

```
POST /api/v1/snapshots/{snapshotId}/restore
```

Creates a new sandbox from the snapshot image.

Response `201 Created`:
```json
{
  "id": "new_sandbox_id",
  "image": "sha256:...",
  "status": "running",
  "created_at": "2026-03-03T12:05:00Z",
  "expires_at": "2026-03-03T12:35:00Z"
}
```

### Delete Snapshot

```
DELETE /api/v1/snapshots/{snapshotId}
```

Removes the snapshot image from Docker.

Response `204 No Content`

---

## Ports

### List Ports

```
GET /api/v1/sandboxes/{id}/ports
```

Response `200 OK`:
```json
[
  {
    "sandbox_port": 3000,
    "host_port": 49152,
    "protocol": "tcp"
  }
]
```

### Add / Remove Ports

```
POST   /api/v1/sandboxes/{id}/ports        → 501 Not Implemented
DELETE /api/v1/sandboxes/{id}/ports/{port} → 501 Not Implemented
```

Port mappings are **fixed at sandbox creation** via the `ports` field in the create
request. They are published **Docker-natively to `127.0.0.1` only, and only in
`network_mode=bridge`** — inert in `internal` (the default), rejected in `none`. There
is no runtime add/remove, so both mutation endpoints permanently return `501`.
To reach a port that was not published, use the sandbox proxy below.

---

## Sandbox Proxy

```
ANY /api/v1/sandboxes/{id}/proxy/{port}/{path...}
```

Forwards HTTP and WebSocket traffic to `{port}` on the sandbox's own address,
through the authenticated control plane. Nothing is published on the host, so any
port works at any time. The rest of the path and the query string are passed
through; `X-API-Key` and `Authorization` are stripped before the request reaches
the sandbox. Proxy traffic counts as sandbox activity for the idle timeout, and a
sandbox does not expire while a proxied request or WebSocket is open. A browser
WebSocket upgrade must come from one of `server.allowed_origins` (requests without
an `Origin` header, such as SDKs and server-side clients, are not checked).

| Status | Meaning |
|--------|---------|
| `400` | Invalid port, or the sandbox has no address (not running) |
| `403` | WebSocket upgrade from an origin that is not allowed |
| `404` | Sandbox not found |
| `502` | Nothing answered on that port |

The proxy dials the sandbox IP from the Guino process, so it needs Guino to run
next to the Docker bridge (native Linux, or inside the same VM as the daemon).
Proxied responses are content served by the sandbox on the API's origin: front it
with your own origin before showing it in a browser that also holds Guino state.

---

## Sessions

Long-lived shells, backed by tmux inside the sandbox. `exec` is one-shot; a session
keeps a server or a build running between calls. Names match
`^[A-Za-z0-9_-]{1,64}$`. Images without tmux answer `501`.

```
POST   /api/v1/sandboxes/{id}/sessions               {"name": "web"}      → 201
GET    /api/v1/sandboxes/{id}/sessions                                    → 200 ["web"]
POST   /api/v1/sandboxes/{id}/sessions/{name}/exec   {"command": "..."}   → 202
GET    /api/v1/sandboxes/{id}/sessions/{name}/logs?lines=500              → 200 {"name", "output"}
DELETE /api/v1/sandboxes/{id}/sessions/{name}                             → 204
```

`exec` types the command into the session and returns at once; read its output
back with `logs`. An unknown session is `404`, a duplicate name on create `409`.

---

## Statistics

### Sandbox Stats

```
GET /api/v1/sandboxes/{id}/stats
```

Response `200 OK`:
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

### System Stats

```
GET /api/v1/stats
```

Response `200 OK`:
```json
{
  "total_sandboxes": 5,
  "running_sandboxes": 3,
  "stopped_sandboxes": 2,
  "total_snapshots": 2
}
```

---

## Error Responses

All errors follow a consistent format:

```json
{
  "error": "description of what went wrong"
}
```

| Status | Meaning |
|--------|---------|
| `400` | Bad request (invalid JSON, missing required fields) |
| `401` | Unauthorized (missing or invalid API key) |
| `404` | Resource not found (sandbox, snapshot, file) |
| `408` | Request timeout (exec exceeded timeout) |
| `409` | Conflict (sandbox is not running) |
| `413` | Payload too large (body > 1MB for JSON, > 100MB for uploads) |
| `422` | Unprocessable (host bind refused, sandbox image not present) |
| `429` | Too many requests (rate limit exceeded) |
| `500` | Internal server error |
| `501` | Not implemented (port mutation, sessions on an image without tmux) |
| `502` | Bad gateway (nothing answered on a proxied port) |
| `503` | Service unavailable (sandbox limit reached) |

## Rate Limiting

When rate limiting is enabled, the API enforces per-key limits. Requests exceeding the limit receive `429 Too Many Requests`.

Rate limit is tracked by API key (when auth is enabled) or by client IP (when auth is disabled).

Default: 10 requests/second with burst of 20.

Sandbox proxy traffic (`/sandboxes/{id}/proxy/...`) has its own bucket per client,
`server.rate_limit_proxy_rps` / `server.rate_limit_proxy_burst` (default 200 / 400),
because one preview page is dozens of asset requests. Set them to `0` to reuse the
control-plane limits.
