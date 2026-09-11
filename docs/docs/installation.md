# Installation

Guino currently installs from this source checkout. Published Guino binaries, npm/PyPI packages, Homebrew and an installation domain are pending. See the [migration status](../migration.md) before assuming a remote installation command is available.

## Requirements

- Go **1.25.7 or newer**, as declared in `go.mod`.
- A running Docker daemon with Linux container support and access to its socket.
- Internet access for initial Go dependencies and image downloads, or an existing offline cache.

Guino does not need a cloud account. Docker Desktop or another local Linux VM supplies the Linux container host on macOS. Docker topology affects networking; read the [security guide](security.md).

## Build from this checkout

```bash
go build -o bin/guino ./cmd/guino
./bin/guino version
```

A direct source build reports a development version unless release metadata is supplied. `make build` also builds `bin/guino`. Keep the absolute path to this binary for MCP clients, whose working directory and PATH may differ from your shell.

## Prepare a sandbox image

For the quickstart:

```bash
docker pull ubuntu:24.04
```

The optional default image includes development tools:

```bash
docker build -t guino/default:latest images/default/
```

`guino/default:latest` is a locally built image, not a promise of a published registry image. Existing installations may keep an explicit `sandbox.default_image` setting. For offline operation, build or pull every required image before disconnecting.

## Optional server container

Build the server image locally:

```bash
docker build -t guino-server:local .
```

Running the server inside Docker requires carefully granting access to a Docker daemon and persistent state. Access to that socket grants host-level container control; review the [self-hosting guide](self-hosting.md) first.

Continue with [Quick Start](quick-start.md) or [MCP setup](mcp.md).
