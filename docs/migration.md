# Migrating Den to Guino

Guino continues the sandbox runtime from [us/den](https://github.com/us/den).
The migration branch starts at upstream commit
`b4d049f6b331fbbb73143e65dfc78a9b359dbabd`. Original commits, authors, branches
and the five existing tags (`v0.0.1`, `v0.0.3` through `v0.0.6`) are preserved.
Historical changelog entries and license notices are unchanged.

## Repository status

The local migration uses a verified Git mirror because the available account
can read, but cannot administer or transfer, `us/den`. This is a history-preserving
copy, not a completed GitHub repository transfer. Upstream remains at its original
location. Issues, pull requests, stars, release assets, settings and redirects do
not migrate with Git refs. No claim is made that upstream has ceased development
or that its maintainers have renamed their repository.

Before any publication, recheck source refs and the destination. A source freeze
would require the upstream owner's cooperation. Do not use an unrestricted
`git push --mirror` against a repository that has gained work in the meantime.

## Compatibility through Guino 0.1.x

| Den | Guino | Transition |
| --- | --- | --- |
| `den` binary | `guino` | Build and install the new binary; the old executable is not overwritten. |
| `den.yaml` | `guino.yaml` | Guino discovers `guino.yaml` first; falls back to `den.yaml` with a stderr warning. Explicit `--config` still works. |
| `DEN_*` settings | `GUINO_*` settings | Legacy names remain readable; matching Guino names take precedence, including explicitly empty values. |
| `github.com/us/den` | `github.com/tmls-ai/guino` | Update Go module and import paths in new code. Historical tags remain unchanged. |
| `@us4/den` | `@tmls-ai/guino` | Source-install the renamed SDK until its first package publication. `Guino` is primary; `Den` remains an alias. |
| `den-sdk`, `import den` | `guino`, `import guino` | The Python distribution and primary import become `guino`; compatibility imports remain available. |
| `den/default:latest` | `guino/default:latest` | Build with `make docker-image`, or explicitly configure an image already present locally. |

Persisted runtime identifiers intentionally retain their Den spellings: `den.db`,
`den-net`, `den.*` Docker labels, existing container and volume prefixes, and
snapshot image references. Renaming these would disconnect existing resources
from their metadata. Do not bulk-rename them. Stop Den before opening its database
with Guino, back up the database and volumes, and retain the old binary for rollback.
Do not run `serve` and standalone `mcp` against the same database concurrently.

The root AGPL-3.0 license is unchanged. SDK manifests retain their inherited
license metadata; this migration does not relicense any component. Original
copyright and author notices remain in place.

## First Guino release

`v0.1.0` is the intended first Guino version, after upstream `v0.0.6`. It is a
release target, not a statement that packages or binaries have been published.
Both proposed registry names were unregistered when checked; that does not reserve
them or prove publishing authority. No Homebrew formula or Guino installation
domain is assumed. Follow the source-build instructions in the README today.

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
3. With destination Actions disabled as described above, publish the agreed
   original refs and reviewed migration commits without force
   pushes or rewriting history. Preserve upstream attribution and licenses.
4. After ref import and the Actions re-enable check, configure npm/PyPI
   credentials and the release token. Review the release PR and run the release
   workflow for its exact Guino tag.
   The first release is `v0.1.0`; remove the temporary `release-as` override after
   it ships. Release-please updates the root release manifest and changelog;
   maintainers must update the TypeScript package version, Python project version
   and `guino.__version__`, both SDK lockfiles, and the MCP advertised version in
   the release PR. The release guard checks the package versions before publishing.
5. Test the released installer, archive checksums, SDK packages and image from a
   fresh environment before announcing availability.

Old packages, domains and upstream settings can only be changed by their owners.
No deprecation notices, transfer, registry publication or external announcement
are implied by preparing this source tree.
