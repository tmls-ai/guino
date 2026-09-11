# Custom Agents and Generic MCP Clients

## Generic MCP client

Complete [MCP setup](mcp.md). Configure a local **stdio** process with:

| Setting | Value |
|---------|-------|
| Command | Absolute path to the built `guino` binary |
| Arguments | `mcp`, `--config`, absolute path to `guino-mcp.yaml` |
| Transport | stdio; JSON-RPC 2.0 |
| Environment | Docker access and any required `GUINO_*` overrides |

The client should initialize the MCP connection, discover tools with `tools/list`, and call tools through `tools/call`. Guino advertises protocol `2024-11-05`. It does not expose an HTTP/SSE MCP endpoint. Keep stdout clear of wrapper-script logging.

A minimal adoption check is `create_sandbox` → `exec` → `destroy_sandbox`. Give `create_sandbox` an image already available on the Docker host, such as `ubuntu:24.04`, and run `["echo", "Hello from Guino"]` with `exec`.

## Your application

For an application that manages execution directly, run the API as described in [Quick Start](quick-start.md), then use the [Go, TypeScript or Python SDK](sdks.md) or the [REST API](rest-api.md). WebSocket execution streams output over the same API service.

Choose one runtime owner for a set of resources. The SDKs talk to `serve`; `mcp` runs its own engine and does not attach to that server. Store API keys outside committed files and configure authentication before sharing an API endpoint.
