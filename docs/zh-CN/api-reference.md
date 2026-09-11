---
title: API 参考
---

# API 参考

S3/MinIO 示例要求运维者在服务端设置 `s3.endpoint` 和 `s3.allow_internal_endpoint: true`（仅在可信的私有端点需要此选项），并提供有效凭据。请求中应省略 `endpoint`，继承服务端端点；启用此例外后不允许逐请求覆盖端点。

[English](../api-reference.md) | **中文**

所有端点位于 `/api/v1/` 路径下。除特别说明外，响应格式均为 JSON。

## 认证

启用认证后，需在请求中包含 API 密钥头：

```
X-API-Key: your-secret-key
```

未认证请求返回 `401 Unauthorized`。

## 健康检查与版本

### 健康检查

```
GET /api/v1/health
```

响应 `200 OK`：
```json
{"status": "ok"}
```

### 版本信息

```
GET /api/v1/version
```

响应 `200 OK`：
```json
{
  "version": "0.0.2",
  "commit": "abc1234",
  "build_date": "2026-03-03T00:00:00Z"
}
```

---

## 沙箱

### 创建沙箱

```
POST /api/v1/sandboxes
Content-Type: application/json
```

请求体：
```json
{
  "image": "ubuntu:22.04",
  "env": {"MY_VAR": "value"},
  "workdir": "/home/sandbox",
  "timeout": "30m",
  "cpu": 1000000000,
  "memory": 536870912,
  "ports": [
    {"sandbox_port": 3000, "host_port": 0, "protocol": "tcp"}
  ]
}
```

| 字段 | 类型 | 默认值 | 描述 |
|------|------|--------|------|
| `image` | string | `guino/default:latest` | 使用的 Docker 镜像 |
| `env` | object | `{}` | 环境变量 |
| `workdir` | string | `""` | 工作目录 |
| `timeout` | int | `1800` | 自动过期时间（秒，默认 30 分钟） |
| `cpu` | int | `0` | CPU 限制，NanoCPU（1核 = 1e9） |
| `memory` | int | `0` | 内存限制，字节（默认 0，无上限） |
| `ports` | array | `[]` | 端口映射（`host_port: 0` 自动分配） |

响应 `201 Created`：
```json
{
  "id": "d6jcj6a9qf76oti2r2sg",
  "image": "ubuntu:22.04",
  "status": "running",
  "created_at": "2026-03-03T11:44:25.809Z",
  "expires_at": "2026-03-03T12:14:25.809Z"
}
```

错误响应：
- `400` — 请求体无效
- `429` — 超出速率限制
- `503` — 达到沙箱最大数量限制

### 列出沙箱

```
GET /api/v1/sandboxes
```

响应 `200 OK`：
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

### 获取沙箱

```
GET /api/v1/sandboxes/{id}
```

响应 `200 OK`：返回沙箱详情 JSON

错误：`404` — 沙箱未找到

### 停止沙箱

```
POST /api/v1/sandboxes/{id}/stop
```

停止容器但不移除。沙箱可查看但不能执行操作。

响应 `200 OK`：
```json
{"status": "stopped"}
```

### 销毁沙箱

```
DELETE /api/v1/sandboxes/{id}
```

停止并移除容器及所有关联状态。

响应 `204 No Content`

---

## 命令执行

### 同步执行

```
POST /api/v1/sandboxes/{id}/exec
Content-Type: application/json
```

请求体：
```json
{
  "cmd": ["python3", "-c", "print('hello')"],
  "env": {"KEY": "value"},
  "workdir": "/tmp",
  "timeout": 30
}
```

| 字段 | 类型 | 默认值 | 描述 |
|------|------|--------|------|
| `cmd` | string[] | **必填** | 命令及参数 |
| `env` | object | `{}` | 附加环境变量 |
| `workdir` | string | `""` | 沙箱内的工作目录 |
| `timeout` | int | `30` | 超时时间（秒，最大 300） |

响应 `200 OK`：
```json
{
  "exit_code": 0,
  "stdout": "hello\n",
  "stderr": ""
}
```

错误响应：
- `400` — 无效请求（空命令）
- `404` — 沙箱未找到
- `409` — 沙箱未在运行

### WebSocket 流式执行

```
GET /api/v1/sandboxes/{id}/exec/stream
Upgrade: websocket
```

通过 WebSocket 连接后发送 JSON 消息：

```json
{"cmd": ["python3", "script.py"], "timeout": 60}
```

服务器流式返回：
```json
{"type": "stdout", "data": "输出行\n"}
{"type": "stderr", "data": "错误行\n"}
{"type": "exit", "data": "0"}
```

---

## 文件操作

通过 `path` 查询参数指定文件路径。路径必须为绝对路径。

可写位置（默认安全配置下）：`/tmp`、`/home/sandbox`、`/run`、`/var/tmp`

### 读取文件

```
GET /api/v1/sandboxes/{id}/files?path=/tmp/hello.py
```

响应 `200 OK`：原始文件内容

### 写入文件

```
PUT /api/v1/sandboxes/{id}/files?path=/tmp/hello.py

print("Hello World!")
```

请求体为原始文件内容。父目录自动创建。

响应 `200 OK`：
```json
{"success": true}
```

### 列出目录

```
GET /api/v1/sandboxes/{id}/files/list?path=/tmp
```

响应 `200 OK`：
```json
[
  {"name": "hello.py", "path": "/tmp/hello.py", "size": 21, "mode": "-rw-r--r--", "is_dir": false},
  {"name": "data", "path": "/tmp/data", "size": 4096, "mode": "drwxr-xr-x", "is_dir": true}
]
```

### 创建目录

```
POST /api/v1/sandboxes/{id}/files/mkdir?path=/tmp/mydir
```

响应 `204 No Content`

### 删除文件或目录

```
DELETE /api/v1/sandboxes/{id}/files?path=/tmp/hello.py
```

响应 `204 No Content`

### 上传文件（Multipart）

```
POST /api/v1/sandboxes/{id}/files/upload?path=/tmp/uploaded.bin
Content-Type: multipart/form-data
```

最大上传大小：100MB

响应 `204 No Content`

### 下载文件

```
GET /api/v1/sandboxes/{id}/files/download?path=/tmp/hello.py
```

返回带 `Content-Disposition: attachment` 头的文件内容。

---

## 快照

> **注意：** 存储在 tmpfs 挂载点（`/tmp`、`/home/sandbox`、`/run`、`/var/tmp`）中的文件**不会被保留**在快照中。Docker commit 捕获容器的可写层，但 tmpfs 存储在内存中，不属于该层。要在快照间持久化文件，请将其写入容器内的非 tmpfs 路径。

### 创建快照

```
POST /api/v1/sandboxes/{id}/snapshots
Content-Type: application/json
```

请求体：
```json
{"name": "安装依赖后"}
```

响应 `201 Created`：返回快照 ID 和元数据

### 列出快照

```
GET /api/v1/sandboxes/{id}/snapshots
```

### 恢复快照

```
POST /api/v1/snapshots/{snapshotId}/restore
```

从快照镜像创建新沙箱。

响应 `201 Created`：返回新沙箱信息

### 删除快照

```
DELETE /api/v1/snapshots/{snapshotId}
```

响应 `204 No Content`

---

## 端口

### 列出端口

```
GET /api/v1/sandboxes/{id}/ports
```

### 新增 / 删除端口

```
POST   /api/v1/sandboxes/{id}/ports        → 501 Not Implemented
DELETE /api/v1/sandboxes/{id}/ports/{port} → 501 Not Implemented
```

端口映射在**创建沙箱时固定**（创建请求的 `ports` 字段），由 Docker 原生发布，
**仅绑定到 `127.0.0.1`，且仅在 `network_mode=bridge` 下生效** —— 在 `internal`
（默认）下惰性，在 `none` 下被拒绝。没有用户态转发器，也不支持运行时增删，
因此两个变更端点永久返回 `501`。

---

## 统计信息

### 沙箱统计

```
GET /api/v1/sandboxes/{id}/stats
```

响应 `200 OK`：
```json
{
  "cpu_percent": 2.5,
  "memory_usage": 15728640,
  "memory_limit": 536870912,
  "pid_count": 3
}
```

### 系统统计

```
GET /api/v1/stats
```

响应 `200 OK`：
```json
{
  "total_sandboxes": 5,
  "running_sandboxes": 3,
  "stopped_sandboxes": 2,
  "total_snapshots": 2
}
```

---

## 错误响应

所有错误遵循统一格式：

```json
{"error": "错误描述"}
```

| 状态码 | 含义 |
|--------|------|
| `400` | 请求错误 |
| `401` | 未认证 |
| `404` | 资源未找到 |
| `408` | 请求超时 |
| `409` | 冲突（沙箱未运行） |
| `413` | 请求体过大 |
| `429` | 请求过多（速率限制） |
| `500` | 服务器内部错误 |
| `503` | 服务不可用（沙箱限制已满） |
