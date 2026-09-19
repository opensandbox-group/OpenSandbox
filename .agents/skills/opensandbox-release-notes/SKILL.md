---
name: opensandbox-release-notes
version: 2.0.0
description: Generate umbrella release notes for the whole OpenSandbox repository (docs/releases/X.Y.Z.md) by analyzing all merged PRs since the previous release baseline. Triggered when users ask to write or generate OpenSandbox release notes, changelogs, or release summaries for a specific version.
---

# OpenSandbox Umbrella Release Notes Generator

Generate the hand-authored release notes for a whole-platform (umbrella) OpenSandbox release: `docs/releases/X.Y.Z.md`, from the template at `docs/releases/TEMPLATE.md`. The umbrella is the only release train — one version `X.Y.Z` covers server, all component images, charts, CLI, and every SDK, so the note must classify changes **by component**, not by repository.

The note is the **authoritative** copy: the `release-umbrella.yml` workflow consumes it verbatim for the BOM commit and mirrors it to the GitHub Release. Preflight fails if the file is missing or empty, so the file must exist and be complete before a release is triggered.

## Parameters

| Placeholder | Example | Description |
|---|---|---|
| `<version>` | `1.1.0` | The umbrella version being released (`X.Y.Z` or `X.Y.Z-rc.N`) |
| `<owner>/<repo>` | `opensandbox-group/OpenSandbox` | GitHub repository (verify with `git remote -v`, prefer `upstream`) |
| `<baseline>` | `release-1.0.0` or per component | What users upgrade FROM. **First look for the previous umbrella release tag `release-X.Y.Z`; only when none exists (first umbrella release) fall back to each component's last independent legacy release tag.** Ask the user only if a baseline cannot be derived |

### Baseline derivation

Priority order:

1. **Previous umbrella tag (the normal case).** Find the highest
   existing `release-X.Y.Z` tag below `<version>` (`git tag -l 'release-*'` / `gh release list`). If one exists, that single tag is the baseline for **every** section — test all PRs against it and move on.
2. **No previous umbrella tag (first umbrella release, e.g. `1.1.0`
   after the legacy era).** Fall back to the last independent legacy release tag per component (see the "First Umbrella Version of Every Artifact" table in `oseps/0016-*.md`, or `gh release list`):

| Note section | Baseline artifact |
|---|---|
| Server | last `server/v*` release |
| Execd | last `docker/execd/v*` release |
| Networking (egress) | last `docker/egress/v*` release |
| Networking (ingress) | last `docker/ingress/v*` release |
| Controller (controller, task-executor, CRDs) | last `k8s/controller/v*` / `k8s/task-executor/v*` |
| Controller (Helm entries) | last `helm/opensandbox/*` / `helm/opensandbox-controller/*` |
| SDKs | last release per language AND per product (`python/sandbox/v*`, `js/code-interpreter/v*`, `python/mcp/sandbox/v*`, …) |
| Misc CLI | last `cli/v*` release |
| Misc node-agent, docs, CI, deps | no legacy release → include everything in the window |

3. **rc.N**: baseline is the previous rc tag on the same line
   (`release-X.Y.0-rc.(N-1)`), or the line's birth baseline for `rc.1`.

**Prefer git ancestry over dates.** For each PR, resolve its merge commit (`gh pr list --json mergeCommit`) and test it against the baseline tag locally — a date cutoff misclassifies PRs merged on release day:

```bash
git fetch upstream --tags
git merge-base --is-ancestor <merge_commit_sha> <baseline_tag>^{commit} && echo SHIPPED
```

`SHIPPED` = the change already reached users in that release → exclude it from that section. `NEW` = first ships in `<version>` → include.

Rules for edge cases (fallback mode only):

- Multi-label PRs are judged per section: a PR can be SHIPPED for one
  component and NEW for another (e.g. a server fix already released in `server/v0.2.3` whose SDK regen first ships now — include under SDKs only if the client-side change is user-visible, not a pure regen).
- Chart-path PRs are judged against the Helm tag of the chart they
  modify, not the controller tag.
- When a component has no legacy release (node-agent, fast-sandbox,
  server chart), everything in the window is first-ship.
- Mention the differing baselines in the note's opening paragraph or
  Upgrade & Compatibility when a section carries a large frozen delta — upgraders need to know a component jumps several months at once.

## Workflow

### Step 1: Load local context

Read these from the working tree (they define the format — never invent sections):

- `docs/releases/TEMPLATE.md` — the exact section structure
- `docs/releases/*.md` — the most recent existing note(s), to match
  tone and conventions
- `docs/releases/*.yaml` — previous BOM(s): give `metadata.releaseDate`
  for baselines and `compatibility.kubernetes` for the upgrade section
- `oseps/0016-unified-umbrella-release-governance.md` — release rules
  (rc packages are held, cadence, naming) when release-type questions arise

Release type from `<version>`:

- `X.Y.0-rc.N` — pre-release: images published, **all packages held**;
  notes are short (mechanics only, no functional claims), SDK/CLI artifacts and Installation SDK lines are omitted
- `X.Y.0` — line birth; `X.Y.Z (Z>0)` — in-line snapshot

### Step 2: Collect merged PRs since the baseline

Determine the baseline per the priority order above (previous umbrella tag, or — when none exists — the earliest per-component legacy baseline), then pull everything from that point in one call, including bodies and merge commits (avoids per-PR fetches):

```bash
gh pr list --repo "<owner>/<repo>" --state merged --limit 1000 \
  --json number,title,body,mergedAt,author,labels,mergeCommit \
  --search "merged:>=<YYYY-MM-DD>" > /tmp/prs-<version>.json
```

Then classify each PR per section with the ancestry test (Parameters section) — not with the global date.

### Step 3: Classify each PR into template sections

Classify by auto-labels first (`.github/workflows/pr-label-check.yml` infers them from paths), then by changed paths for anything unlabeled:

| Section | Labels | Path fallback |
|---|---|---|
| Server | `component/server` | `server/` |
| SDKs | `sdk/python`, `sdk/js`, `sdk/java`, `sdk/c#`, `sdk/go` | `sdks/**` (language from the path segment) |
| Controller | `component/k8s` | `kubernetes/` (controller, task-executor, CRDs — task-executor and image-committer ship from this codebase) |
| Execd | `component/execd` | `components/execd/` |
| Networking | `component/egress`, `component/ingress` | `components/egress/`, `components/ingress/`, `components/internal/` |
| Fast Sandbox | (none) | `fast-sandbox`/`fsb` paths, `manifests/charts/fast-sandbox`, fsb images |
| Misc | `documentation` and everything else | `cli/`, `docs/`, `oseps/`, `specs/` (when doc-only), `.github/`, `manifests/`, node-agent, CI, deps |

Rules:

- A PR with multiple component labels can appear in more than one
  section — mention it where it matters most, cross-reference `(#NNNN)` in the second place.
- `components/code-interpreter` is the sandbox-image side (out of the
  umbrella, versioned in opensandbox-group/sandbox-images) — its *runtime component* changes go under Misc unless they are clearly server/execd behavior. The code-interpreter **SDK** is in scope and belongs under SDKs.
- Spec changes (`specs/`) that accompany a feature belong to that
  feature's component section, not Misc.
- Skip pure release-mechanics PRs for stable notes only if they carry
  no user-visible change; version-bump PRs get one Misc line.

### Step 4: Extract substance from PR bodies

For every classified PR, read `body` and extract:

- **Breaking changes**: explicit "Breaking" sections/mentions → must
  surface in the component section AND in Upgrade & Compatibility with migration guidance
- **Problem → change → scope**: why it was needed, what changed, what
  is explicitly out of scope (e.g. "Docker only, K8s follow-up")
- **Experimental/unstable**: mark entries **[EXPERIMENTAL]** /
  **[UNSTABLE]** per template conventions
- **Security fixes**: flag explicitly (CVE, sandbox escape, auth bypass)

Fall back to the PR title + diff summary (`gh pr view <num> --repo <owner>/<repo>`) only when the body is empty or uninformative.

### Step 5: Draft the note

Copy `docs/releases/TEMPLATE.md` structure exactly; delete section headers with no entries; strip all HTML comments. Sections in order: title `# OpenSandbox <version>`, one-paragraph summary, `## Highlights` (1–3 bullets max), then component sections (Server, SDKs, Controller, Execd, Networking, Fast Sandbox), `## Misc`, `## Upgrade & Compatibility`, `## 👥 Contributors`, `## Artifacts`, `## Installation`.

Writing rules:

- **No hard line wrapping** — one paragraph or one bullet per line, no
  matter how long; markdown renderers join them anyway.
- **Within a component: Features first, then Bug Fixes.** Split the
  Networking section into `### egress` and `### ingress` subsections (shared items go at the end of the more relevant one, labeled). Split the SDKs section into one `### <Language>` subsection per language (Python, JavaScript, Kotlin, C#, Go) with fsb/feature entries distributed to their language; cross-language items stay as lead bullets directly under `## SDKs` — never use a "Cross-SDK" subheading. Cross-SDK changes always go first.
- **Do not copy PR titles** — synthesize; each entry is tight and
  dense: problem → change → key scope, in 1–3 sentences, no filler.
- **Group only PRs that are the same story.** Cite several `(#NNNN)`
  in one entry only when they implement one user story (e.g. the same feature across languages, or one coordinated fix campaign). Never bundle unrelated fixes into a "misc correctness" entry just because they share a component — give each its own bullet instead.
- **Reference every PR** as `(#NNNN)` at the end of its entry
  (space-separated for groups); never full URLs — GitHub auto-links.
- **User-facing** — what operators/consumers need, not internals.
- The opening paragraph states the headline and who should upgrade;
  for a first umbrella / GA (e.g. `1.1.0`), also state the unified-versioning context and the `1.0.0` gap rationale per the umbrella governance OSEP.
- rc notes omit: SDK/CLI package claims, `## Installation` SDK lines,
  `## 👥 Contributors` (optional), and say packages ship at the stable bump.

Fixed boilerplate (fill from the BOM/compat facts, keep wording):

- Upgrade & Compatibility: images `opensandbox/<component>:release-<version>`
  (all three registries; fsb images for fast-sandbox); packages at `<version>`; charts render from the tag; Kubernetes supported range from `compatibility.kubernetes` (latest BOM if no new one); skew sentence (server ↔ CLI/SDK same line supported, ±1 minor warns).
- Artifacts: BOM link `docs/releases/<version>.yaml`; image/package
  inventory lines from the template.
- Installation: verbatim from the template with `<version>` substituted.

### Step 6: Contributors

Deduplicate authors across all final PRs; exclude bots (`dependabot[bot]`, `github-actions[bot]`, `app/github-actions`, etc.):

```bash
jq -r '[.[].author.login] | unique | .[]' /tmp/prs-<version>.json
```

Write one bullet per contributor as a GitHub mention, matching the format of previous releases:

```
- @<username>
```

Filter bots from the list before writing.

### Step 7: Write the file

Write to `docs/releases/<version>.md` in the repo working tree (this is the authoritative in-repo copy the workflow later commits to the release branch). Verify:

- No HTML comments remain; no empty sections remain
- Every entry cites `(#NNNN)`; every collected PR appears exactly once
  (or is deliberately skipped as noise — say so in the handoff)
- Title and all version strings match `<version>` exactly

Print the file path when done.

### Step 8: Stop — do not release

This skill only **generates** `docs/releases/<version>.md`. Do NOT:

- write or hand-edit `docs/releases/<version>.yaml` — the BOM is
  workflow-generated by `create-umbrella-release.sh`
- commit to a release branch, create/push tags (`release-X.Y.Z`, Go
  companion tags), or run `gh release create`
- trigger `release-umbrella.yml`

Hand off: tell the user the note must be committed on the release branch before triggering the workflow, and that they may edit the file freely before then.

## Example

Input: version `1.1.0` — no previous umbrella tag exists, so baselines fall back to the last legacy releases (controller `v0.2.0`/May, ingress `v1.0.10`/Jul, CLI `v0.1.1`/May, server & execd & python Aug 26, js/csharp/go Jul 24, kotlin Aug 25). For `1.1.1`, the baseline would simply be the `release-1.1.0` tag for every section. Output: `docs/releases/1.1.0.md` with Highlights, component sections covering every first-ship PR per component (ancestry-tested against each component's baseline tag), Upgrade & Compatibility (Kubernetes v1.24–v1.34), Contributors, Artifacts, Installation — ready to commit on `release-1.1` before the umbrella workflow runs.
