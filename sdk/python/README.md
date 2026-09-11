# guino

Python SDK for [Guino](https://github.com/tmls-ai/guino) — the self-hosted sandbox runtime for AI agents.

## Installation

The distribution name is `guino`, with Python imports from `guino`. It is not
published yet. Install from a local Guino checkout:

```bash
python -m pip install ./sdk/python
```

## Quick Start

```python
from guino import Guino

client = Guino("http://localhost:8080", api_key="your-key")

# Create a sandbox
sandbox = client.sandbox.create(image="python:3.12-slim")

# Execute a command
result = sandbox.exec(["python3", "-c", "print('Hello from Guino!')"])
print(result.stdout)  # Hello from Guino!

# Read/write files
sandbox.write_file("/tmp/hello.py", "print('hello world')")
content = sandbox.read_file("/tmp/hello.py")

# List files
files = sandbox.list_files("/tmp")

# Clean up
sandbox.destroy()
```

## Async Support

```python
import asyncio
from guino import Guino

async def main():
    client = Guino("http://localhost:8080", api_key="your-key")

    sandbox = await client.sandbox.acreate(image="ubuntu:22.04")
    result = await sandbox.aexec(["echo", "async works!"])
    print(result.stdout)
    await sandbox.adestroy()

asyncio.run(main())
```

## Storage

```python
from guino import Guino, StorageConfig, VolumeMount

client = Guino("http://localhost:8080")

# Persistent volume
sandbox = client.sandbox.create(
    image="ubuntu:22.04",
    storage=StorageConfig(
        volumes=[VolumeMount(name="my-data", mount_path="/data")]
    ),
)
```

## Snapshots

```python
# Save state
snapshot = sandbox.snapshot(name="after-setup")

# Restore from snapshot
restored = client.sandbox.restore_snapshot(snapshot.id)
```

## Sandbox Management

```python
# List all sandboxes
sandboxes = client.sandbox.list()

# Get a specific sandbox
sb = client.sandbox.get("sandbox-id")

# Stop a sandbox
sandbox.stop()

# Get sandbox stats
stats = sandbox.stats()
```

## Features

- Sandbox lifecycle management (create, list, get, stop, destroy)
- Command execution with timeout and environment variables
- File operations (read, write, list, mkdir, delete)
- Persistent volumes, shared volumes, tmpfs configuration
- Snapshot/restore
- Port forwarding
- Async support via `httpx`
- Type-safe with Pydantic models

## Compatibility

Deprecated client and error aliases and import paths remain available through
0.1.x. Before upgrading an existing installation, follow the
[Compatibility and upgrades](../../docs/migration.md) instructions to avoid
conflicting packages that provide the same import paths.

## Requirements

- Python >= 3.10
- Guino server running (see [Guino repo](https://github.com/tmls-ai/guino))

## License

The [SDK package metadata](pyproject.toml) declares MIT. The Guino runtime is licensed
under [AGPL-3.0](../../LICENSE).
