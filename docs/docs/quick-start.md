# Quick Start

This walkthrough uses a trusted local machine, a Docker image with `/bin/sh`, and explicit CPU/memory limits. Complete [Installation](installation.md) first and run commands from the repository root.

## 1. Start the API server

```bash
GUINO_SERVER__HOST=127.0.0.1 \
GUINO_RUNTIME__DEFAULT_NETWORK_MODE=none \
GUINO_SANDBOX__DEFAULT_CPU=1000000000 \
GUINO_SANDBOX__DEFAULT_MEMORY=536870912 \
./bin/guino serve
```

The API and dashboard run at `http://127.0.0.1:8080`. The sandbox has only its own loopback network interface. Authentication is disabled by default, so use this example only on a trusted local machine. Existing configuration files and environment overrides still apply.

The unconfigured server uses `0.0.0.0`, authentication off and `internal` networking; the bind guard intentionally refuses that combination. The explicit loopback/`none` settings above avoid bypassing that guard.

## 2. Create and use a sandbox

In another terminal, from the repository root:

```bash
sandbox_id=$(./bin/guino create --image ubuntu:24.04 --timeout 300)
./bin/guino exec "$sandbox_id" -- sh -c 'echo Hello from Guino'
./bin/guino ls
./bin/guino stats
```

The create command prints the sandbox ID. `--timeout` is its lifetime in **seconds**. The CLI `stats` command reports counts; detailed per-sandbox resource stats are available through the REST API.

## 3. Read and write files

Use the same second terminal so `sandbox_id` remains set:

```bash
curl -fsS -X PUT "http://127.0.0.1:8080/api/v1/sandboxes/$sandbox_id/files?path=/tmp/hello.txt" \
  --data-binary 'Hello from a file'
curl -fsS "http://127.0.0.1:8080/api/v1/sandboxes/$sandbox_id/files?path=/tmp/hello.txt"
```

`/tmp` is a writable tmpfs. Its contents are temporary and are not captured in image snapshots. Use persistent volumes or export data when it must survive sandbox destruction.

## 4. Clean up

```bash
./bin/guino rm "$sandbox_id"
```

The running engine also expires sandboxes after their configured lifetime. Graceful API server shutdown destroys its running sandboxes. See [Core Concepts](concepts.md) for snapshot and volume lifecycle details.

## 5. Connect an agent

Stop the API server before reusing its database/resources with `guino mcp`. MCP runs its own engine over stdio; it is not an HTTP client of `serve`. Follow [MCP setup](mcp.md), then ask your agent:

> Create an Ubuntu 24.04 sandbox with no network, execute `echo Hello from Guino`, show the output, then destroy the sandbox.

For programmatic clients, see [SDKs](sdks.md), [REST API](rest-api.md) and [Custom Agents](custom-agents.md).
