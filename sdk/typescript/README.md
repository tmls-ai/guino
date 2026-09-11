# @tmls-ai/guino

TypeScript SDK for [Guino](https://github.com/tmls-ai/guino) — the self-hosted sandbox runtime for AI agents.


## Installation

The intended package name is `@tmls-ai/guino`. It is not published as part of
this migration. Build from a local Guino checkout with Bun:

```bash
cd sdk/typescript
bun install --frozen-lockfile
bun run build
npm pack
```

From your application directory, install the generated archive (adjust the path):

```bash
npm install /path/to/guino/sdk/typescript/tmls-ai-guino-0.1.0.tgz
```

## Quick Start

```typescript
import { Guino } from "@tmls-ai/guino";

const client = new Guino({ url: "http://localhost:8080", apiKey: "your-key" });

// Create a sandbox
const sandbox = await client.sandbox.create({ image: "python:3.12-slim" });

// Execute a command
const result = await sandbox.exec(["python3", "-c", "print('Hello from Guino!')"]);
console.log(result.stdout); // Hello from Guino!

// Read/write files
await sandbox.writeFile("/tmp/hello.py", "print('hello world')");
const content = await sandbox.readFile("/tmp/hello.py");

// List files
const files = await sandbox.listFiles("/tmp");

// Clean up
await sandbox.destroy();
```

## Storage

```typescript
import { Guino } from "@tmls-ai/guino";

const client = new Guino({ url: "http://localhost:8080" });

// Persistent volume
const sandbox = await client.sandbox.create({
  image: "ubuntu:22.04",
  storage: {
    volumes: [{ name: "my-data", mountPath: "/data" }],
  },
});
```

## Snapshots

```typescript
// Save state
const snapshot = await sandbox.snapshot("after-setup");

// Restore from snapshot
const restored = await client.sandbox.restoreSnapshot(snapshot.id);
```

## Sandbox Management

```typescript
// List all sandboxes
const sandboxes = await client.sandbox.list();

// Get a specific sandbox
const sb = await client.sandbox.get("sandbox-id");

// Stop a sandbox
await sandbox.stop();

// Get sandbox stats
const stats = await sandbox.stats();

// Delete a snapshot
await client.sandbox.deleteSnapshot(snapshot.id);
```

## Features

- Sandbox lifecycle management (create, list, get, stop, destroy)
- Command execution with timeout and environment variables
- File operations (read, write, list, mkdir, delete)
- Persistent volumes, shared volumes, tmpfs configuration
- Snapshot/restore
- Port forwarding
- Full TypeScript types

## Migrating from Den

Import from `@tmls-ai/guino` and use `Guino` and `GuinoError`. The deprecated
`Den` and `DenError` exports remain exact aliases during 0.1.x, including
`instanceof` checks. No old npm package is published or deprecated by this
source migration. HTTP routes, authentication headers, and sandbox behavior
are unchanged.

## Requirements

- Node.js >= 18 or Bun
- Guino server running (see [Guino repo](https://github.com/tmls-ai/guino))

## License and origin

This SDK preserves the MIT designation in its upstream Den package metadata.
The Guino runtime repository retains its [AGPL-3.0 license](../../LICENSE).
Guino continues the original [Den project](https://github.com/us/den); the rename
does not change the SDK license metadata or the project’s history and authorship.
