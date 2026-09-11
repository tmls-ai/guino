# Guino Roadmap

Guino develops the local sandbox runtime inherited from Den. The source history, attribution and existing licenses remain intact. This roadmap records intent, not release promises.

## Guino 0.1.0 preparation

- Complete the public CLI/module/package rename and temporary compatibility aliases.
- Verify source builds, SDKs, MCP tool use and Docker-backed execution/cleanup.
- Publish accurate local onboarding, security boundaries and contributor guidance.
- Resolve repository administration, package ownership and release infrastructure before publishing artifacts.

The [migration status](docs/migration.md) records completed work and release gates. Original Den tags and changelog entries remain historical records.

## Local runtime priorities

- Strengthen integration coverage for snapshots, persistent/shared volumes, storage and cleanup.
- Improve startup diagnostics and first-run MCP ergonomics.
- Investigate controlled egress beyond the current `internal`/`bridge`/`none` modes, without claiming filtering that is not implemented.
- Improve resource-limit guidance based on reproducible measurements.
- Review the legacy compatibility period after Guino 0.1.x and announce removals before shipping them.

## Future managed compute

Managed fleet capacity, scheduling, billing and commercial services may live in separate repositories later. They are outside this migration. Local execution must remain independently useful, with no account requirement or placeholder cloud commands.
