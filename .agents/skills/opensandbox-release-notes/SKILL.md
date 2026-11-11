---
name: opensandbox-release-notes
version: 2.1.0
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

1. **Previous umbrella tag (the normal case).** Find the highest existing `release-X.Y.Z` tag below `<version>` (`git tag -l 'release-*'` / `gh release list`). If one exists, that single tag is the baseline for **every** section — test all PRs against it and move on.
2. **No previous umbrella tag (first umbrella release, e.g. `1.1.0` after the legacy era).** Fall back to the last independent legacy release tag per component (see the "First Umbrella Version of Every Artifact" table in `oseps/0016-*.md`, or `gh release list`):

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

3. **rc.N**: baseline is the previous rc tag on the same line (`release-X.Y.Z-rc.(N-1)`), or the line's birth baseline for `rc.1` (e.g. `1.2.1-rc.1` → `release-1.2.0`).

**Prefer git ancestry over dates.** For each PR, resolve its merge commit (`gh pr list --json mergeCommit`) and test it against the baseline tag locally — a date cutoff misclassifies PRs merged on release day. `SHIPPED` = the change already reached users in that release → exclude it from that section. `NEW` = first ships in `<version>` → include. The batch pipeline lives in Step 2.

Rules for edge cases (fallback mode only):

- Multi-label PRs are judged per section: a PR can be SHIPPED for one component and NEW for another (e.g. a server fix already released in `server/v0.2.3` whose SDK regen first ships now — include under SDKs only if the client-side change is user-visible, not a pure regen).
- Chart-path PRs are judged against the Helm tag of the chart they modify, not the controller tag.
- When a component has no legacy release (node-agent, fast-sandbox, server chart), everything in the window is first-ship.
- Mention the differing baselines in the note's opening paragraph or Upgrade & Compatibility when a section carries a large frozen delta — upgraders need to know a component jumps several months at once.

## Workflow

### Step 1: Load local context

Read these from the working tree (they define the format — never invent sections):

- `docs/releases/TEMPLATE.md` — the exact section structure
- `docs/releases/*.md` — the most recent existing note(s), to match tone and conventions
- `docs/releases/*.yaml` — previous BOM(s): give `metadata.releaseDate` for baselines and `compatibility.kubernetes` for the upgrade section
- `oseps/0016-unified-umbrella-release-governance.md` — release rules (rc packages are held, cadence, naming) when release-type questions arise

Release type from `<version>`:

- `X.Y.Z-rc.N` (any Z) — pre-release: images published, **all packages held**; notes cover the full functional delta but omit SDK/CLI artifact claims and `## Installation` SDK lines, and say packages ship at the stable bump
- `X.Y.0` — line birth; `X.Y.Z (Z>0)` — in-line snapshot

### Step 2: Collect merged PRs since the baseline

Determine the baseline per the priority order above (previous umbrella tag, or — when none exists — the earliest per-component legacy baseline), then pull everything from that point in one call, including bodies and merge commits (avoids per-PR fetches):

```bash
gh pr list --repo "<owner>/<repo>" --state merged --limit 1000 \
  --json number,title,body,mergedAt,author,labels,mergeCommit \
  --search "merged:>=<YYYY-MM-DD>" > /tmp/prs-<version>.json
```

Then classify each PR per section with the ancestry test (Parameters section) — not with the global date. Proven pipeline (writes a SHIPPED/NEW status file, then derives the NEW working set):

```bash
jq -r '.[] | "\(.number) \(.mergeCommit.oid)"' /tmp/prs-<version>.json | while read num sha; do
  if git merge-base --is-ancestor "$sha" <baseline_tag>^{commit} 2>/dev/null; then echo "$num SHIPPED"; else echo "$num NEW"; fi
done > /tmp/prs-<version>-status.txt

NUMS=$(grep NEW /tmp/prs-<version>-status.txt | awk '{print $1}' | paste -sd, -)
jq -r --argjson keep "[$NUMS]" 'map(select(.number as $n | $keep | index($n))) | sort_by(.number) | .[] |
  "\(.number)\t\([.labels[].name] | join(","))\t\(.title)"' /tmp/prs-<version>.json
```

Note: `jq --argfile` is not supported by all builds — pass the number list via `--argjson keep "[$NUMS]"`.

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

- A PR with multiple component labels can appear in more than one section — mention it where it matters most, cross-reference `[#NNNN](…)` in the second place.
- `components/code-interpreter` is the sandbox-image side (out of the umbrella, versioned in opensandbox-group/sandbox-images) — its *runtime component* changes go under Misc unless they are clearly server/execd behavior. The code-interpreter **SDK** is in scope and belongs under SDKs.
- Spec changes (`specs/`) that accompany a feature belong to that feature's component section, not Misc.
- Skip pure release-mechanics PRs for stable notes only if they carry no user-visible change; version-bump PRs get one Misc line.

### Step 4: Extract substance from PR bodies

For every classified PR, read `body` and extract:

- **Breaking changes**: explicit "Breaking" sections/mentions → must surface in the component section AND in Upgrade & Compatibility with migration guidance
- **Problem → change → scope**: why it was needed, what changed, what is explicitly out of scope (e.g. "Docker only, K8s follow-up")
- **Experimental/unstable**: mark entries **[EXPERIMENTAL]** / **[UNSTABLE]** per template conventions
- **Security fixes**: flag explicitly (CVE, sandbox escape, auth bypass)

Fall back to the PR title + diff summary (`gh pr view <num> --repo <owner>/<repo>`) only when the body is empty or uninformative.

Read bodies in bulk, not per-PR: dump all NEW PRs' title+body into one file and read it in chunks — one `jq` call, then `Read` with offsets:

```bash
jq -r --argjson keep "[$NUMS]" 'map(select(.number as $n | $keep | index($n))) | sort_by(.number) | .[] |
  "=====\nPR \(.number) | \(.title)\n-----\n\(.body // "(no body)")\n"' /tmp/prs-<version>.json > /tmp/pr-bodies.txt
```

### Step 4b: Verify pinned third-party upstream changes against the upstream repo

Fast Sandbox is vendored from `opensandbox-group/fast-sandbox` via a pin file; the OpenSandbox PRs that bump it ("chore(manifests): bump fast-sandbox pin to …") may summarize upstream changes in their bodies — **do not trust those summaries as the source of truth**, and some pin-bump PRs carry no upstream summary at all (just a SHA + CRD-sync note). Either way, the upstream git log is the only reliable source; derive the substance yourself from it:

1. Resolve the pin: read `manifests/third-party/fast-sandbox.commit` (repo URL + pinned SHA) and the previous pin from the PR diff (or git history of that file). The local checkout `.fast-sandbox/src` (created by `manifests/release/build-fast-sandbox.sh`) is usually already at the pin — confirm with `git -C .fast-sandbox/src rev-parse HEAD`; fetch inside it if needed.
2. List the real range: `git -C .fast-sandbox/src log --oneline <old-pin>..<new-pin>` and check every claim in the PR body ("Upstream highlights" etc.) maps to a real commit subject; note the commits that carry `!`/`BREAKING CHANGE` — they must appear in the note and in Upgrade & Compatibility. Claims that don't resolve to a commit get dropped, not paraphrased.
3. Spot-check semantic claims in code at the pin, not in the PR text — e.g. `git -C .fast-sandbox/src grep -n '<symbol>' <pin> -- pkg cmd config` (examples: a new controller flag like `--fastlet-proxy-image`, env-file semantics like `sandbox-init.env`, a CRD plural like `sandboxsnapshots`).
4. Check the chart/manifest adaptation matches the upstream change: e.g. after the `vmlinux.bin` drop, no stale `kernelPath`/`kernelURL` default may remain in `manifests/charts/fast-sandbox`; after the CEL drop (`x-kubernetes-validations` removed upstream), the vendored `manifests/charts/base/files/fast-sandbox-crds.yaml` must contain no CEL rules — beware grep hits that are only explanatory comments.
5. Confirm the vendored CRD bundle is in sync with the pin: run `manifests/release/build-fast-sandbox.sh --no-build --sync-crds` and require `git diff --stat manifests/charts/base/files/fast-sandbox-crds.yaml` to be empty; a dirty diff means the pin bump PR's sync claim was wrong — flag it in the handoff.

The same rule applies to any other pinned third-party source under `manifests/third-party/`.

**Writing rule for pin bumps:** the note carries the upstream project's real behavior changes, not the pin mechanics — never lead a bullet with "pin bump to `<sha>`"; write the fsb feature/behavior as the story and treat the bump as provenance. Never write the pin SHA walk ("bump the pinned fast-sandbox source from `<old>` to `<new>`") or CRD-bundle sync/verification details. Read the real substance (upstream commit bodies are often empty — prefer the docs/diff changes the commits touch, e.g. `git -C .fast-sandbox/src diff <old>..<new> -- docs/`), group it into one bullet per distinct story (problem → behavior change → why it matters), and on each bullet cite the OpenSandbox pin-bump PR as carrier **plus a link to the corresponding upstream PR**, e.g. `([#2546](…/OpenSandbox/pull/2546) [fast-sandbox#158](https://github.com/opensandbox-group/fast-sandbox/pull/158))`. Map stories to upstream PRs via the merge commits (`git -C .fast-sandbox/src log --merges --oneline <old>..<new>`) and attribute each commit to the **earliest** upstream merge that contains it — stacked PRs contain each other's commits, so ancestry alone over-attributes (`git merge-base --is-ancestor <commit> <merge_sha>` per candidate). Pull OpenSandbox-side fsb PRs (template/init wiring, integration env, e2e defaults) into this section too — the section is "everything fsb", not only the pin deltas.

### Step 5: Draft the note

Copy `docs/releases/TEMPLATE.md` structure exactly; delete section headers with no entries; strip all HTML comments. The note has no H1 title — the GitHub Release supplies it. Sections in order: one-paragraph summary, `## Highlights` (1–3 bullets max), then component sections (Server, SDKs, Controller, Execd, Networking, Fast Sandbox), `## Misc`, `## Upgrade & Compatibility`, `## 👥 Contributors`, `## Artifacts`, `## Installation`.

Writing rules:

- **No hard line wrapping** — one paragraph or one bullet per line, no matter how long; markdown renderers join them anyway.
- **Within a component: Features first, then Bug Fixes.** Split the Networking section into `### egress` and `### ingress` subsections (shared items go at the end of the more relevant one, labeled). Split the SDKs section into one `### <Language>` subsection per language (Python, JavaScript, Kotlin, C#, Go) with fsb/feature entries distributed to their language; cross-language items stay as lead bullets directly under `## SDKs` — never use a "Cross-SDK" subheading. Cross-SDK changes always go first.
- **Concise and dense.** Never copy PR titles — synthesize. Major entries (headline features, breaking changes, security fixes, user-visible failure stories) get at most 2 sentences (problem → change → impact); minor bugfixes get exactly one sentence ("what broke → now fixed") and should be folded into a single trailing "smaller fixes" bullet per component — semicolon-separated, each with its own `[#NNNN]` citation. Trivial means: no data-loss/security angle, no operator reaction, no migration step.
- **Group only PRs that are the same story.** Cite several `[#NNNN](…pull/NNNN)` links in one entry only when they implement one user story (e.g. the same feature across languages, or one coordinated fix campaign) — or when they are individually trivial one-liners folded together per the density-gradient rule above. Never bundle unrelated *substantive* fixes into a "misc correctness" entry just because they share a component — give each its own bullet instead.
- **Reference every PR** as a full markdown link `[#NNNN](https://github.com/<owner>/<repo>/pull/NNNN)` at the end of its entry (space-separated for groups, each inside one shared parenthesis).
- **User-facing** — what operators/consumers need, not internals.
- The opening paragraph carries only what Highlights cannot: line context (rc or stable cut, baseline), who should upgrade, and package/image shipping timing — never enumerate headline features (that is `## Highlights`' job). For a first umbrella / GA, also state the unified-versioning context per the umbrella governance OSEP.
- rc notes omit: SDK/CLI package claims, `## Installation` SDK lines, `## 👥 Contributors` (optional), and say packages ship at the stable bump.
- **Upgrade & Compatibility never gets the density gradient** — every behavior change, breaking change, deprecation, and migration step keeps its own complete bullet there, even when its component-section entry was compressed to one sentence.

Fixed boilerplate (fill from the BOM/compat facts, keep wording):

- Upgrade & Compatibility: images `opensandbox/<component>:release-<version>` (all three registries; fsb images for fast-sandbox); packages at `<version>`; charts render from the tag; Kubernetes supported range from `compatibility.kubernetes` (latest BOM if no new one); skew sentence (server ↔ CLI/SDK same line supported, ±1 minor warns).
- Artifacts: BOM link `docs/releases/<version>.yaml`; image/package inventory lines from the template.
- Installation: verbatim from the template with `<version>` substituted.

### Step 6: Contributors

Deduplicate authors across the NEW PRs only (not the SHIPPED ones in the same file); exclude bots (`dependabot[bot]`, `github-actions[bot]`, `app/github-actions`, etc.):

```bash
jq -r --argjson keep "[$NUMS]" '[.[] | select(.number as $n | $keep | index($n)) | .author.login] | unique | .[]' /tmp/prs-<version>.json
```

Also include the authors of every upstream PR linked in the note (e.g. fast-sandbox pin-bump carriers: `gh pr view <N> --repo opensandbox-group/fast-sandbox --json author`), deduplicated into the same list; these upstream authors usually also appear as OpenSandbox contributors, so additions are rare — add only the genuinely new logins.

Write one bullet per contributor as a GitHub mention, matching the format of previous releases:

```
- @<username>
```

Filter bots from the list before writing.

### Step 7: Write the file

Write to `docs/releases/<version>.md` in the repo working tree (this is the authoritative in-repo copy the workflow later commits to the release branch). Verify:

- No HTML comments remain; no empty sections remain
- Every entry cites a `[#NNNN](…pull/NNNN)` link; every collected PR appears exactly once (or is deliberately skipped as noise — say so in the handoff)
- No bare `#NNNN` references remain — all are full markdown links to `https://github.com/<owner>/<repo>/pull/NNNN`
- Title and all version strings match `<version>` exactly

Run the definitive completeness check — the cited-PR set must equal the NEW set exactly:

```bash
grep -oE "pull/[0-9]+" docs/releases/<version>.md | sort -u | sed 's/pull\///' > /tmp/cited.txt
grep NEW /tmp/prs-<version>-status.txt | awk '{print $1}' | sort > /tmp/expected.txt
diff /tmp/cited.txt /tmp/expected.txt && echo OK
```

**Refresh after new merges:** PRs may land between drafting and the release cut. Re-run Step 2, diff the PR numbers against `/tmp/cited.txt` to isolate the delta, and add only the new PRs — full re-verification afterwards.

Print the file path when done.

### Step 8: Stop — do not release

This skill only **generates** `docs/releases/<version>.md`. Do NOT:

- write or hand-edit `docs/releases/<version>.yaml` — the BOM is workflow-generated by `create-umbrella-release.sh`
- commit to a release branch, create/push tags (`release-X.Y.Z`, Go companion tags), or run `gh release create`
- trigger `release-umbrella.yml`

Hand off: tell the user the note must be committed on the release branch before triggering the workflow, and that they may edit the file freely before then.

Related prep (only on explicit request, not part of note generation): the platform version bump across charts/values/Chart.lock/chart READMEs is `manifests/release/bump-versions.sh --version <version>` — it rewrites and **auto-commits**; on rc it leaves SDK/CLI versions untouched (they bump at the stable release). It requires a clean tracked worktree (untracked notes don't block).
