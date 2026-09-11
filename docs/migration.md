# Compatibility and upgrades

Guino 0.1.x accepts legacy configuration and SDK aliases so existing installations
can upgrade without disconnecting their data. Use Guino names for new code and
consult the mappings below when upgrading an existing setup.

## Compatibility through Guino 0.1.x

| Previous identifier | Guino | Transition |
| --- | --- | --- |
| `den` binary | `guino` | Build and install the new binary; the old executable is not overwritten. |
| `den.yaml` | `guino.yaml` | Guino discovers `guino.yaml` first; falls back to `den.yaml` with a stderr warning. Explicit `--config` still works. |
| `DEN_*` settings | `GUINO_*` settings | Legacy names remain readable; matching Guino names take precedence, including explicitly empty values. |
| `github.com/us/den` | `github.com/tmls-ai/guino` | Update Go module and import paths in new code. Historical tags remain unchanged. |
| `@us4/den` | `@tmls-ai/guino` | Source-install the renamed SDK until its first package publication. `Guino` is primary; `Den` remains an alias. |
| `den-sdk`, `import den` | `guino`, `import guino` | The Python distribution and primary import become `guino`; compatibility imports remain available. |
| `den/default:latest` | `guino/default:latest` | Build with `make docker-image`, or explicitly configure an image already present locally. |

Persisted runtime identifiers retain their existing spellings: `den.db`,
`den-net`, `den.*` Docker labels, existing container and volume prefixes, and
snapshot image references. Renaming these would disconnect existing resources
from their metadata. Do not bulk-rename them. Stop the previous runtime before opening its database
with Guino, back up the database and volumes, and retain the old binary for rollback.
Each runtime needs exclusive ownership of its database and managed Docker
resources. Do not run the previous runtime, `serve` or multiple MCP processes
against those resources concurrently. A second database alone does not isolate
Docker ownership.

## SDK compatibility

TypeScript and Python retain `Den` and `DenError` as exact aliases of `Guino`
and `GuinoError` through 0.1.x, including TypeScript `instanceof` checks. Prefer
the Guino names in new code.

Python also supports the compatibility imports `den`, `den.client`,
`den.exceptions`, `den.sandbox` and `den.types`. Uninstall `den-sdk` before
installing Guino in the same Python environment: both distributions provide
the `den` package and must not manage those files simultaneously.

## First Guino release

`v0.1.0` is the intended first Guino release. Packages, binaries, Homebrew and an
installation domain remain pending. Follow the source-build instructions in the
[README](../README.md) today.

The new release workflows require the repository variable
`GUINO_RELEASES_ENABLED=true`. **That variable cannot guard old workflow files
stored in historical tags.** Before importing any original refs, disable GitHub
Actions for the destination repository and withhold publishing credentials.
Verify no inherited workflow runs were queued, then re-enable Actions only after
the reviewed Guino tree is the default branch. Do not rewrite old tags to change
their workflows.

Guino publication runs through the release-please reusable workflow or a manual
dispatch for an exact tag, not a second tag-push trigger. The release guard
rejects legacy source trees and mismatched SDK versions.

Before enabling publication:

1. Review the final diff, compatibility tests and full CI results, including the
   native Linux Docker network/security proof. A macOS Docker run cannot prove it.
2. Verify registry ownership and access without adding publishing credentials
   to Actions yet. Verify GHCR
   permissions, repository metadata, private vulnerability reporting and required
   checks/branch protection. Add `good first issue` and `help wanted` labels if
   absent. These are repository settings, not effects of local files.
3. Configure npm/PyPI credentials and the release token only after the preceding
   checks pass. Review the release PR and run the release
   workflow for its exact Guino tag.
   The first release is `v0.1.0`; remove the temporary `release-as` override after
   it ships. Release-please updates the root release manifest and changelog;
   maintainers must update the TypeScript package version, Python project version
   and `guino.__version__`, both SDK lockfiles, and the MCP advertised version in
   the release PR. The release guard checks the package versions before publishing.
4. Test the released installer, archive checksums, SDK packages and image from a
   fresh environment before announcing availability.

Preserve existing license and copyright notices when distributing Guino. The
runtime license is [AGPL-3.0](../LICENSE); SDK manifests declare MIT.
