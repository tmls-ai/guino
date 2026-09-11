# Guino

**Local infrastructure for AI agents.**

Run agent-generated code in Docker sandboxes on your own machine through a CLI, REST API, SDK or MCP interface. Guino needs no cloud account; once the binary and container images are installed, the local runtime works without internet access.

[Getting started](docs/docs/quick-start.md) · [Connect your agent](docs/docs/mcp.md) · [Documentation](docs/README.md) · [Security](SECURITY.md) · [中文](README.zh-CN.md)

Guino is derived from [Den](https://github.com/us/den), with its Git history, authorship and existing license preserved. This checkout prepares the Guino relaunch; it does not imply that the upstream repository has transferred or redirects here. See the [migration status](docs/migration.md).

## Start locally

Requirements: a running Docker daemon, access to its socket, and Go **1.25.7 or newer** for a source build. Run these commands from this checkout. Package registry releases, Homebrew and an installation domain are pending; the source build is the current installation path.

```bash
go build -o bin/guino ./cmd/guino
docker pull ubuntu:24.04

# Local API with no sandbox networking and explicit resource limits.
GUINO_SERVER__HOST=127.0.0.1 \
GUINO_RUNTIME__DEFAULT_NETWORK_MODE=none \
GUINO_SANDBOX__DEFAULT_CPU=1000000000 \
GUINO_SANDBOX__DEFAULT_MEMORY=536870912 \
./bin/guino serve
```

In another terminal, from the same checkout:

```bash
sandbox_id=$(./bin/guino create --image ubuntu:24.04 --timeout 300)
./bin/guino exec "$sandbox_id" -- sh -c 'echo Hello from Guino'
./bin/guino ls
./bin/guino rm "$sandbox_id"
```

The API and embedded dashboard are at `http://127.0.0.1:8080`. This quickstart is for a trusted local machine: API authentication is disabled by default. Read the [configuration guide](docs/docs/configuration.md) before exposing the server or enabling sandbox networking. Existing configuration or environment overrides still apply.

## Connect your agent with MCP

Guino includes a stdio MCP server. Your agent starts `guino mcp` and can create sandboxes, run commands, manage files and take snapshots. MCP runs the engine directly and does not require `guino serve`.

```text
Claude Code / Codex / Cursor / another MCP client
                    ↓ stdio
                 guino mcp
                    ↓ Docker
               local sandboxes
```

Follow the [MCP setup](docs/docs/mcp.md) for the dedicated store and configuration, then use the guide for [Claude Code](docs/docs/claude-code.md), [Codex](docs/docs/codex.md), [Cursor](docs/docs/cursor.md) or a [generic client](docs/docs/custom-agents.md). Each runtime process needs exclusive ownership of its database and managed Docker resources; stop `serve` before switching that runtime to MCP.

## What you can do

| Capability | Included |
|---|---|
| Sandbox lifecycle | Create, list, inspect, stop and destroy Docker containers; automatic expiry |
| Execution | Command output and exit status over REST; streaming over WebSocket |
| Files | Read, write, list, upload and download sandbox files |
| Snapshots | Docker image snapshots and restore; tmpfs and mounted-volume contents are separate |
| Storage | Persistent/shared Docker volumes, configurable tmpfs, S3 import/export and hooks; optional FUSE |
| Resources | CPU, memory and PID limits, host pressure monitoring and throttling |
| Clients | CLI, Go, TypeScript and Python SDKs, and 11 MCP tools |
| Operations | Embedded dashboard, API key authentication, rate limits and TLS configuration |

All of these run on infrastructure you control. Guino Cloud is outside this migration; there are no account or billing dependencies in the local execution path.

## Isolation and trust

Guino is intended for local development and self-hosted environments with a trusted operator. Containers share the Docker host's kernel. They are not a universal security boundary for mutually hostile tenants.

The runtime drops all Linux capabilities and adds back `NET_BIND_SERVICE`, `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE` and `FOWNER`. Root filesystems are read-only by default, with writable tmpfs/explicit mounts, `no-new-privileges` and a default PID limit of 256. CPU and memory defaults are **unlimited** unless configured; the quickstart sets explicit limits.

The default `internal` network still reaches the Docker bridge gateway, embedded DNS and host services. `none` disables external networking (container loopback remains); it does not remove kernel or shared-volume risks. Bridge mode enables unfiltered egress and requires an explicit opt-in. The [security model](SECURITY.md) documents these boundaries, authentication, Docker topology and S3 SSRF protection in detail.

## SDKs and documentation

[Go, TypeScript and Python SDK guide](docs/docs/sdks.md) includes source installation instructions. The intended package identities are `github.com/tmls-ai/guino`, `@tmls-ai/guino` and Python `guino`; publishing is pending.

- [Getting started](docs/docs/quick-start.md) and [installation](docs/docs/installation.md)
- [Core concepts](docs/docs/concepts.md): sandboxes, execution, files, networking, snapshots, volumes and storage
- [Configuration](docs/docs/configuration.md), [resource management](docs/docs/resources.md) and [self-hosting](docs/docs/self-hosting.md)
- [REST API](docs/api-reference.md), [CLI](docs/cli.md) and [MCP tools](docs/docs/mcp.md)
- [Contributing](CONTRIBUTING.md), [roadmap](ROADMAP.md) and [release history](CHANGELOG.md)

## Project history and license

The original Den commits, tags, authors and historical changelog remain intact. Guino changes are new work on that foundation. Legacy configuration and SDK aliases are documented in the [migration guide](docs/migration.md).

The runtime remains **AGPL-3.0**; see [LICENSE](LICENSE). Existing SDK package metadata retains its original MIT declarations. This rename does not relicense existing code or transfer contributors' copyright.
