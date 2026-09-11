# Core Concepts

## Sandboxes

A sandbox is a managed Docker container plus metadata in BoltDB. Creation selects an image, lifetime and resource limits. The engine tracks lifecycle state, rejects creation under critical memory pressure and reaps expired sandboxes while running.

The default image `guino/default:latest` must be built locally, or choose an image explicitly. Guino does not add Python to an arbitrary Ubuntu image: choose an image containing the tools your command needs.

## Execution

REST execution returns stdout, stderr and an exit code. WebSocket execution streams output. The CLI forwards the sandbox command's exit status. Commands receive argument arrays; use a shell explicitly when you need shell syntax.

## Files

File operations address absolute paths inside the sandbox. Root filesystems are read-only by default; `/tmp`, `/home/sandbox`, `/run` and `/var/tmp` use writable tmpfs mounts unless configured otherwise. Path validation rejects traversal and null bytes. Keep important output in a persistent volume or export it before cleanup.

## Networking

The global modes are `internal` (default), `bridge` and `none`. A sandbox can inherit the mode or request `none`. `internal` prevents normal NAT egress but still permits access to the Docker gateway, embedded DNS and host services. `bridge` enables unfiltered egress and optional ports on the Docker daemon's loopback. Ports are fixed at creation and work only in bridge mode. `none` disables external networking (container loopback remains); containers still share a kernel.

See [Configuration](configuration.md) for the bind guard and platform restrictions.

## Snapshots

Snapshots use Docker image commits and metadata. They are not VM checkpoints: process memory, tmpfs contents and mounted volume data are not included. Restoring creates a new sandbox. Persistent volumes require their own data lifecycle and backups.

The CLI supports snapshot create and restore. Listing/removing snapshots is available through the REST API/SDKs; there is no CLI `snapshot ls` command.

## Volumes and storage

Named Docker volumes survive sandbox destruction and can be mounted read/write or read-only across sandboxes. Sharing data also shares a trust boundary. Temporary files use configurable tmpfs; optional S3 import/export and hooks move data to object storage. S3 FUSE requires an explicit opt-in.

Guino uses `den-` volume prefixes, the `den-net` managed network, `den.*` ownership labels and the `den.db` store name as persisted runtime identifiers. Keep these names intact when upgrading so the runtime can find its existing state and resources.

See [Architecture](architecture.md) for runtime details and [REST API](rest-api.md) for storage configuration and endpoints.
