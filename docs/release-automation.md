# Generic Release Automation

This repository uses tag-driven publish workflows. The script below standardizes:

- canonical tag creation for each release target
- release note generation from previous release to current commit
- GitHub Release create/update
- signed source archive upload and provenance attestation in the Generic
  Release workflow

Script path:

- `scripts/release/create-release.sh`

## Supported Targets

- `js/sandbox`
- `js/code-interpreter`
- `python/sandbox`
- `python/code-interpreter`
- `python/mcp/sandbox`
- `java/sandbox`
- `java/code-interpreter`
- `csharp/sandbox`
- `csharp/code-interpreter`
- `sdks/sandbox/go`
- `cli`
- `server`
- `docker/execd`
- `docker/code-interpreter`
- `docker/ingress`
- `docker/egress`
- `k8s/controller`
- `k8s/task-executor`
- `helm/opensandbox`
- `helm` (alias of `helm/opensandbox`)

## Tag Rules

The script aligns with existing workflow triggers:

- v-prefixed tags:
  - `<target>/v<version>` for SDK/CLI/Server targets
  - examples: `js/sandbox/v1.0.5`, `server/v0.2.0`
  - Go SDK example: `sdks/sandbox/go/v1.0.0`
- plain suffix tags:
  - `<target>/<version>` for docker/k8s/helm targets
  - examples: `docker/execd/v0.3.0`, `helm/opensandbox/0.1.0`

## Release Notes Format

Generated notes follow `docs/RELEASE_NOTE_TEMPLATE.md` sections:

- `## What's New`
- `### ✨ Features`
- `### 🐛 Bug Fixes`
- `### ⚠️ Breaking Changes`
- `### 📦 Misc`
- `## 👥 Contributors`

Commit categorization:

- `feat:` -> Features
- `fix:` -> Bug Fixes
- `BREAKING CHANGE` or `type!:` -> Breaking Changes
- everything else -> Misc

## Usage

```bash
scripts/release/create-release.sh --target <target> --version <version> [options]
```

Required:

- `--target`
- `--version`

Options:

- `--from-tag <tag>`: explicit previous release boundary
- `--path <path>`: append custom path filter (repeatable)
- `--no-path-filter`: disable default target path scope and use whole range
- `--initial-release`: allow no previous tag; use full history
- `--dry-run`: render computed tag/range/notes without side effects
- `--push`: push created tag to origin
- `--sign-tag`: create a cryptographically signed git tag using the local git
  signing configuration. This is intended for local release-operator use, not
  the hosted GitHub Actions release workflow.

## Path Filtering Strategy

By default, each target only includes commits from target-related paths to reduce noise.

Examples:

- `js/sandbox` -> `sdks/sandbox/javascript` + `specs/sandbox-lifecycle.yml`
- `server` -> `server` + `specs/sandbox-lifecycle.yml`
- `docker/egress` -> `components/egress`
- `helm/opensandbox` -> `kubernetes/charts/opensandbox`

Override behavior:

- Add extra scope with `--path`:
  - `--path docs/` or `--path specs/execd-api.yaml`
- Disable default scope with `--no-path-filter`:
  - falls back to the entire commit range (`from..HEAD`)

## Common Examples

Dry-run JavaScript SDK release:

```bash
scripts/release/create-release.sh --target js/sandbox --version 1.0.5 --dry-run
```

Dry-run server release:

```bash
scripts/release/create-release.sh --target server --version 0.2.0 --dry-run
```

Dry-run JavaScript SDK release with additional docs scope:

```bash
scripts/release/create-release.sh --target js/sandbox --version 1.0.5 --dry-run --path docs/
```

Dry-run JavaScript SDK release without path filtering (full range):

```bash
scripts/release/create-release.sh --target js/sandbox --version 1.0.5 --dry-run --no-path-filter
```

Server release with tag push:

```bash
scripts/release/create-release.sh --target server --version 0.2.0 --push
```

Component image release:

```bash
scripts/release/create-release.sh --target docker/execd --version v0.3.0 --push
```

Helm chart release:

```bash
scripts/release/create-release.sh --target helm/opensandbox --version 0.1.0 --push
```

## Dry-Run Output Example

Example output format for `--dry-run`:

```text
[release] Target: js/sandbox
[release] Workflow: .github/workflows/publish-js-sdks.yml
[release] New tag: js/sandbox/v1.0.5
[release] Previous tag: js/sandbox/v0.1.4
[release] Path filters: sdks/sandbox/javascript specs/sandbox-lifecycle.yml
[release] Dry run enabled. No tag/release side effects will be performed.
[release] Computed range: js/sandbox/v0.1.4..HEAD

[release] Generated release notes preview:
------------------------------------------------------------
# JavaScript Sandbox SDK v1.0.5
## What's New
Changes included since `js/sandbox/v0.1.4`.
Scoped paths: `sdks/sandbox/javascript specs/sandbox-lifecycle.yml`.

### ✨ Features
- feat(sdks/js): support run_in_session

### 🐛 Bug Fixes
- fix(lifecycle): harden sdk compatibility and e2e stability

### ⚠️ Breaking Changes
- None

### 📦 Misc
- chore(sdks): rebuild source code
------------------------------------------------------------
```

If `--dry-run` is enabled, the script never creates/pushes tags and never creates/updates GitHub Releases.

## Safety Defaults

- The script creates/updates GitHub Release only when not in `--dry-run`.
- Tag push is opt-in (`--push`), preventing accidental workflow trigger.
- Tag signing is opt-in (`--sign-tag`) because it requires release-operator git
  signing keys. The hosted GitHub Actions release workflow does not expose this
  option. Official release artifacts are still signed by the GitHub release
  workflows through Sigstore/GitHub attestations.
- If previous tag cannot be found, script fails unless `--from-tag` or `--initial-release` is provided.

## GitHub Actions Entry

You can trigger the same flow in GitHub Actions from:

- `.github/workflows/release-generic.yml`

Inputs exposed in the workflow dispatch form:

- `target`
- `version`
- `from_tag` (optional)
- `initial_release` (boolean)
- `push_tag` (boolean)
- `dry_run` (boolean, default `true`)

Dry-run in GitHub Actions:

- set `dry_run=true`
- set `push_tag=false`
- check logs for:
  - computed tag (`New tag`)
  - range (`Computed range`)
  - preview body (`Generated release notes preview`)

Recommended first run in UI:

- set `dry_run=true`
- keep `push_tag=false`
- verify the generated release notes preview in logs
- rerun with `dry_run=false` and `push_tag=true` when confirmed

When `dry_run=false`, `.github/workflows/release-generic.yml` uploads an
explicit `opensandbox-<tag>.tar.gz` source archive and `SHA256SUMS` file to the
GitHub Release, then signs both files with GitHub/Sigstore provenance
attestations. See [Release Verification](release-verification.md) for user
verification commands and release signing coverage.
