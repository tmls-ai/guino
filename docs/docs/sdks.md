# SDKs

guino provides official SDKs for Go, TypeScript, and Python.

Published packages are pending. Use the SDKs from this checkout; registry availability alone does not establish publishing ownership.

| SDK | Intended identity | Source setup |
|-----|-------------------|--------------|
| Go | `github.com/tmls-ai/guino/pkg/client` | Use the module in this checkout, or a local `replace` in a consuming Go module |
| TypeScript | `@tmls-ai/guino` | [TypeScript source installation](../../sdk/typescript/README.md) |
| Python | distribution `guino`, import `guino` | `python -m pip install ./sdk/python` from the repository root |

The TypeScript/Python primary classes are `Guino`/`GuinoError`; deprecated `Den`/`DenError` aliases are retained through 0.1.x. Python also retains the old `den` import shim. The SDK package metadata retains its original MIT declarations; the runtime remains AGPL-3.0.

The examples below assume [the API server](quick-start.md) is already running. They are usage fragments; handle errors and clean up resources in your application. Images must contain the invoked executable.

## Go SDK

For a separate local application module, add `replace github.com/tmls-ai/guino => /absolute/path/to/guino` to its `go.mod`; replace the path with this checkout.

```go
import client "github.com/tmls-ai/guino/pkg/client"

c := client.New("http://localhost:8080",
    client.WithAPIKey("your-api-key"),
)

// Create sandbox (timeout in seconds)
sb, _ := c.CreateSandbox(ctx, client.SandboxConfig{
    Image:   "python:3.12-slim",
    Timeout: 1800, // 30 minutes
})

// Execute command
result, _ := c.Exec(ctx, sb.ID, client.ExecOpts{
    Cmd: []string{"python3", "-c", "print('hello')"},
})
fmt.Println(result.Stdout)

// File operations
c.WriteFile(ctx, sb.ID, "/tmp/test.py", []byte(`print("world")`))
content, _ := c.ReadFile(ctx, sb.ID, "/tmp/test.py")

// Snapshots
snap, _ := c.CreateSnapshot(ctx, sb.ID, "checkpoint")
restored, _ := c.RestoreSnapshot(ctx, snap.ID)

// Cleanup
c.DestroySandbox(ctx, sb.ID)
c.DestroySandbox(ctx, restored.ID)
```

### Methods

| Method | Description |
|--------|-------------|
| `CreateSandbox(ctx, config)` | Create a new sandbox |
| `GetSandbox(ctx, id)` | Get sandbox details |
| `ListSandboxes(ctx)` | List all sandboxes |
| `StopSandbox(ctx, id)` | Stop a sandbox |
| `DestroySandbox(ctx, id)` | Destroy a sandbox |
| `Exec(ctx, id, opts)` | Execute a command |
| `ReadFile(ctx, id, path)` | Read file contents |
| `WriteFile(ctx, id, path, content)` | Write a file |
| `CreateSnapshot(ctx, id, name)` | Snapshot a sandbox |
| `RestoreSnapshot(ctx, snapshotID)` | Restore from snapshot |
| `Health(ctx)` | Health check |

## TypeScript SDK

```typescript
import { Guino } from '@tmls-ai/guino';

const guino = new Guino({
  url: 'http://localhost:8080',
  apiKey: 'your-api-key',
});

// Create sandbox (timeout in seconds)
const sandbox = await guino.sandbox.create({
  image: 'python:3.12-slim',
  timeout: 1800, // 30 minutes
});

const result = await sandbox.exec(['python3', '-c', 'print("hello")']);
console.log(result.stdout);

await sandbox.writeFile('/tmp/test.py', 'print("world")');
const content = await sandbox.readFile('/tmp/test.py');

// Snapshots
const snapshot = await sandbox.snapshot('checkpoint');
const snapshots = await sandbox.listSnapshots();

await sandbox.destroy();
```

## Python SDK

```python
from guino import Guino

# Sync usage
client = Guino("http://localhost:8080", api_key="your-api-key")

sandbox = client.sandbox.create(image="ubuntu:22.04")
result = sandbox.exec(["echo", "hello"])
print(result.stdout)

sandbox.destroy()
client.close()
```

### Async Usage

```python
import asyncio
from guino import Guino

async def main():
    client = Guino("http://localhost:8080", api_key="your-api-key")

    sandbox = await client.sandbox.acreate(image="ubuntu:22.04")
    result = await sandbox.aexec(["echo", "hello"])
    print(result.stdout)

    await sandbox.adestroy()
    await client.aclose()

asyncio.run(main())
```

## Network mode & ports

All three SDKs accept an optional per-sandbox `network_mode` on the create
config. It may only be `""` (inherit the server's global default) or `"none"`
(no network) — a per-sandbox value may only **increase** isolation. Any other
value, including one equal to the server default, is rejected by the server
with HTTP `400`.

When `network_mode` is set, the SDK performs a **lazy, scoped** capability
probe (`GET /api/v1/version`, cached on first success) and fails fast with a
clear error if the server does not advertise the `network_mode` feature. The
`features` list is a **capability hint only — not an authentication signal**;
servers that predate it return no tokens, and the probe is skipped entirely
when `network_mode` is not used, so older servers keep working for everything
else.

The `ports` field on a returned sandbox is **present iff non-empty**: it is
populated only in `network_mode=bridge` (Docker-native publishing to
`127.0.0.1`), and is absent/empty in `internal` (the default) and `none`,
where publishing is inert. Port mappings are fixed at creation; there is no
runtime add/remove (`POST`/`DELETE /ports` → `501`). Protocol is always
`tcp` — udp is not supported.
