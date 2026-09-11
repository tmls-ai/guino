# Contributing to Guino

Guino builds on Den with preserved history and attribution. Keep the local runtime useful without a cloud account, and keep migration changes on top of existing commits.

## Development Setup

### Prerequisites

- Go 1.25.7+
- Docker running locally
- [golangci-lint](https://golangci-lint.run/usage/install/)

### Building from Source

```bash
# From this source checkout
go build -o bin/guino ./cmd/guino
```

### Running Tests

```bash
# Unit tests
go test ./cmd/... ./internal/... ./pkg/... -short -v

# With race detector
go test ./cmd/... ./internal/... ./pkg/... -race -count=1 -v

# SDK dependencies, tests and package builds (requires Bun and uv)
make test-sdk
```

The Docker integration suite requires cached sandbox images and a seeded MinIO
fixture. On a local test daemon, prepare them from the repository root:

```bash
docker pull busybox:latest
docker pull alpine:latest
docker compose -p guino-integration -f tests/integration/docker-compose.minio.yml up -d --wait minio
docker compose -p guino-integration -f tests/integration/docker-compose.minio.yml run --rm createbuckets
make test-integration
# Remove only this suite's MinIO fixture when finished.
docker compose -p guino-integration -f tests/integration/docker-compose.minio.yml down
```

The fixture publishes localhost ports 9000/9001; use an isolated test host if
they are occupied. Set `MINIO_ENDPOINT` when using another seeded MinIO service.
The full suite includes tests under both `internal/` and `tests/`. Host-published
port tests require the test process to run on the Docker host. CI explicitly
leaves those two proofs to the required `native-host` job when testing a remote
Docker-in-Docker daemon. `make e2e-network` adds real CLI/API network checks;
its positive native-Linux attestation leg cannot be verified on Docker Desktop.

### Running the Server

Follow the [quickstart](docs/docs/quick-start.md) or prepare a configuration with deliberate authentication and network settings. Unconfigured `serve` is intentionally refused by its bind guard.

```bash
./bin/guino serve --config guino.yaml
```

## Pull Request Process

1. Fork the repository and create a feature branch from `main`
2. Branch naming: `feat/description`, `fix/description`, `refactor/description`
3. Write tests for new functionality
4. Ensure all tests pass: `go test ./cmd/... ./internal/... ./pkg/... -race`
5. Run the linter: `golangci-lint run`
6. Commit using [Conventional Commits](https://www.conventionalcommits.org/):
   - `feat:` new features
   - `fix:` bug fixes
   - `refactor:` code restructuring
   - `docs:` documentation
   - `test:` tests
   - `ci:` CI/CD changes
   - `perf:` performance improvements
7. Open a PR against `main` with a clear description

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Use `slog` for structured logging
- Use `context.Context` as the first parameter in functions that do I/O
- Handle errors explicitly — do not ignore them
- Write table-driven tests where appropriate

## Project Structure

```
cmd/guino/          — CLI entry point and commands
internal/
  api/            — HTTP handlers, middleware, WebSocket
  config/         — Configuration loading and validation
  engine/         — Core sandbox lifecycle management
  mcp/            — Model Context Protocol server
  runtime/docker/ — Docker runtime implementation
  storage/        — Volume, tmpfs, and S3 storage
  store/          — BoltDB persistence
  pathutil/       — Path validation utilities
pkg/client/       — Go SDK (public API)
sdk/
  typescript/     — TypeScript SDK
  python/         — Python SDK
```

## Migration and compatibility

Keep historical commits, tags, authors and changelog entries unchanged. `den.db`, `den-net`, `den.*` ownership labels, and snapshot/volume prefixes are intentional compatibility identifiers. Guino 0.1.x retains documented legacy config/environment and SDK aliases; any later removal needs an announced migration path.

Use source installs until release artifacts and package publishing are verified. Update CLI examples against actual `--help`; do not document placeholder cloud or MCP installation commands.

For SDK changes, follow the source setup and checks in [TypeScript](sdk/typescript/README.md) and [Python](sdk/python/README.md). Match validation to the changed behavior and report checks that could not run. Docker-dependent tests require an isolated daemon: do not run destructive integration tests against a daemon holding valuable workloads.

## Reporting Issues

- Use [GitHub Issues](https://github.com/tmls-ai/guino/issues) for bug reports and feature requests
- Include Guino version, OS, Docker version, and reproduction steps
- For security vulnerabilities, see [SECURITY.md](SECURITY.md)

## License

Keep the applicable existing license for the files you change: the runtime is AGPL-3.0, and existing SDK package metadata declares MIT. Do not remove copyright notices or infer that this rename authorizes relicensing or transfers contributor IP. A future contributor agreement or commercial licensing policy requires a separate decision.
