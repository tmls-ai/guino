<p align="center">
  <img src="docs/assets/guino.png" width="280" alt="Guino，黑白色豚鼠吉祥物">
</p>

<h1 align="center">Guino</h1>

**面向 AI 智能体的本地基础设施。**

通过 CLI、REST API、SDK 或 MCP，在自己的机器上运行 Docker 沙箱并执行智能体生成的代码。无需 Guino 云账户；安装好二进制文件和容器镜像后，本地运行时可以离线工作。

[English](README.md) · [快速开始](docs/docs/quick-start.md) · [连接智能体](docs/docs/mcp.md) · [安全模型](SECURITY.md)

Guino 基于 [Den](https://github.com/us/den)，保留原有 Git 历史、作者与许可证。本分支正在准备 Guino 发布，并不表示上游仓库已经转移或重定向。参见[迁移状态](docs/migration.md)。

## 本地运行

需要运行中的 Docker、Docker socket 访问权限，以及 Go **1.25.7 或更新版本**。在本仓库目录中执行：

```bash
go build -o bin/guino ./cmd/guino
docker pull ubuntu:24.04
GUINO_SERVER__HOST=127.0.0.1 \
GUINO_RUNTIME__DEFAULT_NETWORK_MODE=none \
GUINO_SANDBOX__DEFAULT_CPU=1000000000 \
GUINO_SANDBOX__DEFAULT_MEMORY=536870912 \
./bin/guino serve
```

在另一个终端的同一目录中：

```bash
sandbox_id=$(./bin/guino create --image ubuntu:24.04 --timeout 300)
./bin/guino exec "$sandbox_id" -- sh -c 'echo Hello from Guino'
./bin/guino ls
./bin/guino rm "$sandbox_id"
```

API 与仪表板位于 `http://127.0.0.1:8080`。此示例用于可信的本地开发环境，默认关闭 API 认证，沙箱只保留自身的回环网络接口。对外开放服务前，请阅读[配置](docs/docs/configuration.md)和[安全模型](SECURITY.md)。现有配置文件和环境变量仍然生效。

## 通过 MCP 连接智能体

`guino mcp` 通过标准输入输出运行本地引擎，无需先启动 `guino serve`。同一数据库和 Docker 资源只能由一个运行时进程管理；切换到 MCP 前先停止对应的 `serve` 进程。

按 [MCP 配置说明](docs/docs/mcp.md)准备独立数据库和配置文件，然后连接 [Claude Code](docs/docs/claude-code.md)、[Codex](docs/docs/codex.md)、[Cursor](docs/docs/cursor.md)或[通用 MCP 客户端](docs/docs/custom-agents.md)。

## 保留的能力

- Docker 沙箱生命周期管理与自动过期清理
- REST 执行、WebSocket 流式输出、文件读写及上传下载
- Docker 镜像快照与恢复；tmpfs 和挂载卷内容需要单独保存
- 持久卷、共享卷、tmpfs、S3 导入导出与 hooks，以及可选 FUSE
- CPU、内存和 PID 限制，内存压力监测与节流
- Go、TypeScript、Python SDK，11 个 MCP 工具和嵌入式仪表板

## 安全边界

容器共享 Docker 主机内核，不适合作为互不信任租户之间的通用安全边界。根文件系统默认只读，启用 `no-new-privileges`，默认 PID 上限为 256。丢弃全部 Linux capabilities 后会恢复 `NET_BIND_SERVICE`、`CHOWN`、`SETUID`、`SETGID`、`DAC_OVERRIDE` 和 `FOWNER`。

CPU 和内存默认**没有硬性上限**，上面的示例显式设置了限制。默认 `internal` 网络仍可访问桥接网关、内置 DNS 和部分主机服务；`none` 禁用外部网络，仅保留容器自身的回环接口，但无法消除共享内核和共享卷风险。`bridge` 允许未过滤的外连，需要显式开启。

## 安装、贡献与许可

目前从源码安装。npm、PyPI、Homebrew、安装域名和正式发布尚待完成；不要把计划中的包名当作已发布版本。查看 [SDK 指南](docs/docs/sdks.md)、[完整文档](docs/README.md)、[贡献说明](CONTRIBUTING.md)、[路线图](ROADMAP.md)与[历史变更](CHANGELOG.md)。

运行时继续使用 [AGPL-3.0](LICENSE)。现有 SDK 的包元数据保留原有 MIT 声明。此次更名不修改历史许可或贡献者版权。
