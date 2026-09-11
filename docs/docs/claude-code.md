# Claude Code

Prepare the dedicated configuration and binary paths in [MCP setup](mcp.md). Claude Code can launch Guino as a local stdio server:

```bash
claude mcp add --transport stdio --scope user guino -- /absolute/path/guino/bin/guino mcp --config /absolute/path/guino-mcp.yaml
claude mcp list
```

Replace both paths with actual absolute paths. In Claude Code, use `/mcp` to check the server connection. The command follows the [official Claude Code MCP documentation](https://code.claude.com/docs/en/mcp).

For project-scoped setup, use `--scope project` instead of `--scope user`; Claude Code writes `.mcp.json`. Review a project's MCP configuration before trusting it. The Claude Desktop configuration file is a different integration and is not the Claude Code config path.

Ask Claude to create an Ubuntu sandbox, execute `echo Hello from Guino`, show the result and destroy the sandbox. Do not run the same Guino runtime from another client concurrently.
