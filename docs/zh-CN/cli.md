---
title: CLI 参考
---

# CLI 参考

[English](../cli.md) | **中文**

Guino 是一个单一可执行文件，同时作为服务器和 CLI 客户端使用。

## 全局参数

```
--config string    配置文件路径（默认：guino.yaml）
--server string    客户端命令的 API 服务器地址（默认：http://localhost:8080）
```

本指南以 [英文 CLI 参考](../cli.md)为准。`--timeout` 使用整数秒。未经配置的 `serve` 默认组合会被网络安全守卫拒绝，请先完成[快速开始](../docs/quick-start.md)。

服务器地址也可通过 `GUINO_URL` 环境变量设置。

---

## 服务器

### `guino serve`

启动 HTTP API 服务器。

```bash
# 使用默认配置
guino serve

# 使用配置文件
guino serve --config production.yaml
```

按 `Ctrl+C` 优雅关闭（销毁所有运行中的沙箱）。

---

## 沙箱管理

### `guino create`

创建新沙箱。

```bash
# 默认镜像需要在本地构建
guino create

# 自定义镜像和超时
guino create --image python:3.12 --timeout 3600

# 资源限制
guino create --image node:20 --memory 268435456 --cpu 500000000
```

参数：`--image`、`--timeout`、`--cpu`、`--memory`

### `guino ls`

列出所有沙箱。

```
ID                     IMAGE           STATUS    AGE
d6jcj6a9qf76oti2r2sg  ubuntu:22.04    running   5m
```

### `guino exec`

在沙箱中执行命令。

```bash
guino exec d6jcj6a9qf76oti2r2sg -- echo "你好！"
guino exec d6jcj6a9qf76oti2r2sg -- python3 -c "print(2+2)"
guino exec d6jcj6a9qf76oti2r2sg -- bash -c "ls -la /tmp && echo 完成"
```

命令的 stdout 打印到终端。非零退出码反映在 CLI 的退出码中。

### `guino rm`

销毁沙箱（停止并移除）。

```bash
guino rm d6jcj6a9qf76oti2r2sg
```

---

## 快照

### `guino snapshot create`

```bash
guino snapshot create <sandbox-id>
guino snapshot create <sandbox-id> --name "安装后"
```

### `guino snapshot restore`

```bash
guino snapshot restore <snapshot-id>
```

从快照创建新的运行沙箱。

---

## 统计

### `guino stats`

```bash
guino stats
```

仅显示沙箱总数、运行数和停止数。详细资源指标通过 REST API 获取。

---

## MCP 服务器

### `guino mcp`

启动 MCP 服务器（stdio 模式），供 AI 工具使用。

```bash
guino mcp
guino mcp --config guino.yaml
```

参见 [MCP 集成](../docs/mcp.md)。

---

## 版本

### `guino version`

```bash
guino version
# 未注入版本元数据的源码构建：guino dev (commit: none, built: unknown)
```

---

## 退出码

| 代码 | 含义 |
|------|------|
| 0 | 成功 |
| 1 | 一般错误，包括 CLI 用法错误 |

`guino exec` 的退出码与沙箱内命令的退出码一致。

## 环境变量

| 变量 | 描述 |
|------|------|
| `GUINO_URL` | API 服务器地址（默认 `http://localhost:8080`） |
| `GUINO_API_KEY` | 认证 API 密钥 |
| `GUINO_SERVER__PORT` | 覆盖服务器端口 |
| `GUINO_LOG__LEVEL` | 日志级别 |

查看[配置指南](../docs/configuration.md)了解所有环境变量选项。
