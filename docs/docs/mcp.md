# MCP Server

Guino provides 11 tools over JSON-RPC 2.0 on stdin/stdout. The client launches the local binary; Guino logs to stderr. There is no HTTP MCP endpoint and no `mcp install` subcommand.

## Prepare the runtime

Complete [Installation](installation.md), including pulling `ubuntu:24.04`. Save the following as an absolute path such as `/absolute/path/guino-mcp.yaml`, replacing the store path with a real writable path:

```yaml
runtime:
  default_network_mode: none
sandbox:
  default_image: ubuntu:24.04
  default_cpu: 1000000000
  default_memory: 536870912
  default_timeout: 30m
store:
  path: /absolute/path/guino-mcp.db
```

This configuration disables external sandbox networking (container loopback remains) and sets explicit limits. Each runtime needs exclusive ownership of its BoltDB and managed Docker resources. Do not run `serve`, Den or another MCP process against the same database/resources concurrently. Use one client at a time for this setup; a second database alone does not isolate shared Docker ownership labels.

The process command is:

```bash
/absolute/path/guino/bin/guino mcp --config /absolute/path/guino-mcp.yaml
```

Use actual absolute paths in client configuration. The client must be able to access the binary, configuration, writable store directory and Docker socket. A stdio server waiting silently in a terminal is normal: it expects protocol messages from a client.

## Choose your client

- [Claude Code](claude-code.md)
- [Codex](codex.md)
- [Cursor](cursor.md)
- [Generic MCP clients and custom agents](custom-agents.md)

After connecting, ask the agent to create an `ubuntu:24.04` sandbox, execute `echo Hello from Guino`, show the output and destroy the sandbox. Confirm the output and cleanup in the tool results.

## Available tools

| Tool | Parameters | Description |
|------|------------|-------------|
| `create_sandbox` | `image?`, `timeout?`, `cpu?`, `memory?`, `network_mode?` | Create a sandbox; timeout in seconds, CPU in NanoCPUs, memory in bytes |
| `exec` | `sandbox_id`, `cmd`, `env?`, `workdir?`, `timeout?` | Execute command arguments and return output/exit status |
| `read_file` | `sandbox_id`, `path` | Read a file; binary content is base64 encoded |
| `write_file` | `sandbox_id`, `path`, `content` | Write a file |
| `list_files` | `sandbox_id`, `path` | List directory contents |
| `delete_file` | `sandbox_id`, `path` | Delete a file or directory |
| `mkdir` | `sandbox_id`, `path` | Create a directory |
| `destroy_sandbox` | `sandbox_id` | Destroy a sandbox |
| `list_sandboxes` | — | List sandboxes |
| `snapshot_create` | `sandbox_id`, `name?` | Create an image snapshot; tmpfs and mounted volume data are separate |
| `snapshot_restore` | `snapshot_id` | Create a sandbox from a snapshot |

A per-sandbox `network_mode` accepts only an empty value (inherit) or `none`. It cannot weaken the operator's network policy. Bridge mode requires a server-side opt-in and has unfiltered egress; see [Security](security.md).

## Architecture and lifecycle

```text
MCP client → stdio JSON-RPC → guino mcp → Engine → Docker
```

MCP creates its own engine and store directly. It does not require or communicate with `guino serve`; API authentication settings do not authenticate the stdio transport. Trust in the local process and the client's tool approvals controls access. Guino advertises MCP protocol version `2024-11-05`.

Explicitly destroy sandboxes when finished. Do not rely on closing an MCP client to clean everything up: abrupt process exit can leave Docker resources behind, and the expiry loop only runs while the engine is active.

## Troubleshooting

- **Binary not found:** use its absolute path; desktop applications may not inherit shell PATH.
- **Docker connection refused:** start the daemon and verify access with `docker info`. Set `DOCKER_HOST` for an intentional non-default daemon; `runtime.docker_host` is inert.
- **Store lock or stalled startup:** stop the other process that owns the database. Never start two runtime managers for the same Docker resources.
- **Image unavailable:** pull/build it first. The `guino/default:latest` image must be built locally.
- **Protocol error:** keep stdout reserved for MCP; wrapper scripts must send diagnostics to stderr.
- **File or network restrictions:** `/tmp` is writable; the root filesystem is read-only by default and this setup disables sandbox networking.
