# Codex

Prepare the dedicated configuration and binary paths in [MCP setup](mcp.md). Add Guino as a stdio server:

```bash
codex mcp add guino -- /absolute/path/guino/bin/guino mcp --config /absolute/path/guino-mcp.yaml
codex mcp list
```

Alternatively, merge this table into `~/.codex/config.toml`:

```toml
[mcp_servers.guino]
command = "/absolute/path/guino/bin/guino"
args = ["mcp", "--config", "/absolute/path/guino-mcp.yaml"]
```

Replace both paths. Do not duplicate an existing `mcp_servers.guino` table. Codex also supports project-scoped `.codex/config.toml` in trusted projects. Local clients on the same Codex host share MCP configuration; consult the [official OpenAI MCP documentation](https://learn.chatgpt.com/docs/extend/mcp?surface=cli).

Restart/reload the client after changing configuration, and use `/mcp` in the CLI to inspect connected tools. Ask it to create an Ubuntu sandbox, run `echo Hello from Guino`, then destroy the sandbox. The client must have Docker access on the machine where this process runs. Hosted chats do not gain access to your local Docker daemon through this configuration.
