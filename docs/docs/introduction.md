# Guino

**Local infrastructure for AI agents.**

Guino gives agents Docker sandboxes for executing code through a CLI, REST API, WebSocket, SDK or MCP connection. The runtime is self-hosted and remains useful without a commercial service or Guino account.

## Start with your workflow

- [Quick Start](quick-start.md): build the binary, create a sandbox, execute a command and clean up.
- [MCP](mcp.md): let Claude Code, Codex, Cursor or another client manage sandboxes directly.
- [SDKs](sdks.md): integrate local execution into a Go, TypeScript or Python application.
- [Self-hosting](self-hosting.md): configure authentication, networking, state and resource limits.

## Included runtime capabilities

Sandbox lifecycle and automatic expiry; synchronous and streaming execution; file operations; Docker image snapshots; persistent/shared volumes and tmpfs; S3 hooks and import/export; optional FUSE; resource pressure monitoring; and an embedded dashboard.

[Core Concepts](concepts.md) explains what persists and how these features work together. [Architecture](architecture.md) retains the detailed technical design.

## Local independence

The runtime does not phone a commercial service to execute code. Initial installation still needs downloaded dependencies and images, unless you provide them from an offline cache. S3 and other network services remain optional integrations.

## Security scope

Docker containers share the host kernel. Guino's controls reduce risk for local and self-hosted agent execution, but do not provide a universal hostile multi-tenant boundary. CPU and memory are unlimited unless configured; `internal` networking is not full isolation. Read [Security](security.md) before selecting a network mode or exposing the API.
