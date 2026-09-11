# Cursor

Prepare the configuration and absolute paths in [MCP setup](mcp.md). Merge this entry into the project's `.cursor/mcp.json`, or use `~/.cursor/mcp.json` for a user-level configuration:

```json
{
  "mcpServers": {
    "guino": {
      "command": "/absolute/path/guino/bin/guino",
      "args": ["mcp", "--config", "/absolute/path/guino-mcp.yaml"]
    }
  }
}
```

Replace both paths and preserve existing server entries. Check the server in Cursor's MCP settings and enable its tools for your agent. See the [official Cursor MCP documentation](https://cursor.com/docs/context/mcp).

Ask the agent to create an `ubuntu:24.04` sandbox, execute `echo Hello from Guino`, return the output and destroy the sandbox. Use one local client for this runtime at a time; another MCP process can conflict over the database or managed Docker resources.
