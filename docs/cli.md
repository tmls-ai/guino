# CLI Reference

Guino is a single binary with API server, HTTP client and stdio MCP commands. The examples below assume `guino` is on PATH; from a source checkout, use `./bin/guino`.

## Global options

```text
--config string    Runtime configuration file
--server string    API URL for client commands (default: http://localhost:8080)
```

`serve` and `mcp` read the selected configuration. Without `--config`, they look for `guino.yaml`, then deprecated `den.yaml`. Client commands use `--server`, then `GUINO_URL`, then the default URL. `GUINO_API_KEY` supplies the client's API key; it does not enable authentication on the server.

Legacy `DEN_URL`, `DEN_API_KEY` and nested `DEN_*` configuration names remain deprecated aliases through Guino 0.1.x. `GUINO_*` wins, including when explicitly empty. Legacy use warns on stderr.

## guino serve

```bash
guino serve --config guino.yaml
```

Start the REST/WebSocket API and embedded dashboard. The command validates config, connects to Docker, applies network guards, reconciles persisted resources and starts the engine. The unconfigured `0.0.0.0`/auth-off/`internal` combination is refused; follow the [quickstart](docs/quick-start.md) for a working local configuration.

Graceful shutdown destroys running sandboxes. Persistent Docker volumes and image snapshots have separate lifecycles. Only one process may manage a database and its Docker resources.

## guino create

```bash
guino create --image ubuntu:24.04 --timeout 300 --cpu 1000000000 --memory 536870912
```

| Flag | Type | When omitted |
|------|------|--------------|
| `--image` | string | Server default; `guino/default:latest` unless configured |
| `--timeout` | int | Server lifetime default, normally 1800 seconds |
| `--cpu` | int64 | Server CPU default, normally 0/unlimited; NanoCPUs |
| `--memory` | int64 | Server memory default, normally 0/unlimited; bytes |

Creation prints the sandbox ID. `--timeout` takes seconds: use `3600`, not `1h`. The default image must be built locally, or choose an image already available to Docker.

## guino ls

```bash
guino ls
```

Prints a table with `ID`, `IMAGE`, `STATUS` and `AGE`.

## guino exec

```bash
guino exec <sandbox-id> -- echo hello
guino exec <sandbox-id> -- sh -c 'ls -la /tmp'
```

Commands receive arguments after `--`; use a shell explicitly for shell syntax. Stdout/stderr are forwarded, and a nonzero sandbox command exit status becomes the CLI exit status. Choose an image containing the executable you invoke.

## guino rm

```bash
guino rm <sandbox-id>
```

Stops and removes the sandbox. Export temporary files first.

## guino snapshot

```bash
guino snapshot create <sandbox-id> --name checkpoint
guino snapshot restore <snapshot-id>
```

Create prints a snapshot ID; restore prints the new sandbox ID. Snapshots are Docker image snapshots, not process-memory, tmpfs or volume backups. Snapshot listing/deletion is available through the REST API/SDKs; there is no `snapshot ls` CLI subcommand.

## guino stats

```bash
guino stats
```

Prints total, running and stopped sandbox counts. This command does not implement per-sandbox CPU/memory stats. Use the REST API for detailed resource metrics.

## guino mcp

```bash
guino mcp --config /absolute/path/guino-mcp.yaml
```

Runs its own engine and Docker runtime over stdin/stdout. Logs go to stderr. It does not connect to `serve`; stop the other runtime before reusing its resources. See [MCP setup](docs/mcp.md). There is no `mcp install` command.

## guino debug classify-platform

```bash
guino debug classify-platform
```

Prints the Docker topology classifier inputs and verdict for diagnosing network guard refusals.

## guino version

```bash
guino version
```

Prints the binary version, commit and build date. Source builds without injected metadata report a development version.

## Exit status

`0` indicates success; command/setup failures return nonzero. For `exec`, the sandbox command's exit status is propagated.
