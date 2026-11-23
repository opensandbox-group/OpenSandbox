---
name: opensandbox-pr-review
version: 1.1.0
description: Deep-review a GitHub PR in the OpenSandbox repository (评审 PR / review PR / 给我一份评审报告 / 中文报告 / 中英双语报告). Runs OCR tool review (when available) on the PR ref range, independently verifies every factual claim in code and tests (bug reproduction on the base commit, tests on the PR branch inside isolated worktrees), and produces a concise professional report — Markdown in chat plus a standalone HTML with Chinese/English toggle by default. The report leads with a value card sized to the PR type (bugfix — problem, is it real, fix, cost, test coverage; feature — public config and spec changes, compatibility, contract sync, test coverage), followed by severity-graded findings with exact file:line references.
---

# OpenSandbox PR Review (OCR + Manual Verification)

Deep review workflow for a GitHub PR against this repository: tool-assisted OCR review + independent manual verification + reproducible evidence + concise bilingual report organized around the PR's value. Reference example: PR #2979 (execd reply deadline fix).

## Input

| Item | Example | Required |
|---|---|---|
| PR URL or number | `https://github.com/opensandbox-group/OpenSandbox/pull/2979` | yes |
| Review focus / business context | "提出的问题是否成立，性能与兼容性影响" | no (default: full review) |
| Output requirements | 中文报告 / 中英双语 / 仅 Markdown | no (default: Chinese Markdown in chat + bilingual HTML file) |

## Principles

1. **Never trust the PR description.** Every factual claim (file:line, constant value, channel capacity, default timeout, enum) is verified at its definition site in code. Claims that cannot be resolved are dropped or marked as unverified — never paraphrased as fact.
2. **Reproduction first.** For a bug-fix PR, the new regression test must be run against the base commit and shown to fail there. A fix for a bug that cannot be reproduced is a red flag, reported explicitly.
3. **Isolated verification.** All test runs happen in detached worktrees. Never touch the user's working tree (it may carry uncommitted changes); never check out the PR branch in place.
4. **Dual-channel review.** OCR tool review (via `tools.ocr_review` or the local `ocr` CLI) and own reading of the diff + full changed files are complementary. Both run whenever either OCR interface is available; conclusions merge. Only mark OCR skipped in the verification log when neither `tools.ocr_review` nor a local `ocr` binary is available.
5. **Value first, by PR type.** The first block after the verdict is a **value card**: a bugfix answers *what is broken → is it real → how is it fixed → what the fix costs → are the tests sufficient*; a feature answers *which public config / API / spec it adds or changes → defaults and compatibility → is the contract synced → are the tests sufficient*. Everything else in the report only supports this card.
6. **Report discipline.** Verdict first, exact `file:line` references, findings graded by severity, a verification log table, explicit annotation for checks not run locally — and brevity: a card row is at most 2 lines, a finding at most 2 lines, passing edge cases are counted rather than enumerated, and nothing repeats what the card says.

## Workflow

### Step 1: Gather PR metadata

Fetch PR metadata with `gh` (structured JSON — no HTML scraping):

```bash
gh pr view <number> --repo opensandbox-group/OpenSandbox \
  --json title,body,author,baseRefName,baseRefOid,headRefName,headRefOid,headRepository,headRepositoryOwner,commits,files,labels,statusCheckRollup,reviews,comments
```

Extract from the output:

- Title, base branch (`baseRefName`), head branch (`headRefName`) plus pinned commit SHAs (`baseRefOid`, `headRefOid`), head repo/owner, author, commit list, and CI/review status (`statusCheckRollup`, `reviews`, `comments` — helpful for spotting unrun CI jobs or open feedback, still verified independently)
- **PR type**, which sets the review focus and the value card: `bugfix` (`fix(...)` title, "Fixes #N", or a body claiming a defect), `feature` (`feat(...)` title, or changes under `specs/`, public config — flags, env, Helm values, CRDs — or SDK public API), otherwise `other` (refactor / perf / docs / test / chore). A mixed PR takes the type of its main change; the secondary aspect becomes one extra card row.
- A type-specific **claim checklist** from the body — every concrete assertion to verify in later steps:
  - bugfix: what breaks and for whom (trigger, user-visible impact); the reproduction claim (test or command, expected vs actual); the stated fix (mechanism, files); stated testing; the breaking-change statement (e.g. "resultChan holds 10 results", "fails on main with expected 15, actual 11")
  - feature: each public config item added or changed (name, type, default, required?); each spec change (path, operationId, schema field; additive or breaking); each new or changed public SDK symbol in the handwritten adapters (method, class, parameter, default, error semantics); what the author says is synced (docs, generated SDKs, CLI, examples, Helm/CRD output); the migration story

### Step 2: Fetch refs locally

Identify the git remote pointing to `opensandbox-group/OpenSandbox` via `git remote -v` (usually `origin`, or `upstream` when `origin` is a personal fork). Fetch GitHub's numeric `refs/pull/<N>/head` ref and the PR's actual `baseRefName` — never derive a fork URL from `<author>` (which breaks for upstream branches and org-owned forks) and never interpolate an unquoted `<head-branch>` name into shell commands (branch names are untrusted input and may contain shell metacharacters).

```bash
PR_NUM=<number>
REMOTE=origin   # remote for opensandbox-group/OpenSandbox (verify with git remote -v)
BASE_BRANCH=$(gh pr view "$PR_NUM" --repo opensandbox-group/OpenSandbox --json baseRefName --jq .baseRefName)
EXPECTED_HEAD=$(gh pr view "$PR_NUM" --repo opensandbox-group/OpenSandbox --json headRefOid --jq .headRefOid)

git -c core.hooksPath=/dev/null fetch --no-tags --no-recurse-submodules -- "$REMOTE" \
  "+refs/pull/${PR_NUM}/head:refs/remotes/pr/${PR_NUM}" \
  "+refs/heads/${BASE_BRANCH}:refs/remotes/${REMOTE}/${BASE_BRANCH}"

BASE=$(git merge-base "refs/remotes/${REMOTE}/${BASE_BRANCH}" "refs/remotes/pr/${PR_NUM}")
HEAD=$(git rev-parse "refs/remotes/pr/${PR_NUM}")
test "$HEAD" = "$EXPECTED_HEAD"  # abort or re-snapshot if the PR moved during fetch

git log --oneline "$BASE..$HEAD"
git diff --stat "$BASE..$HEAD"
git diff "$BASE..$HEAD"
```

Record `$BASE` and `$HEAD` (`<BASE>..<HEAD>`) — this resolved commit range feeds both manual reading and OCR. If the diff touches `.opencodereview/`, `.agents/`, `.gitattributes`, `.gitmodules`, or `.github/workflows/`, call it out explicitly in the report so reviewers inspect policy/CI changes carefully.

### Step 3: Manual verification of code

- `Read` the **full changed files**, not just the diff hunks. Line numbers in the report must reference the PR-head version.
- For each item in the claim checklist, locate the definition with `grep`/`read` and confirm or refute it (constants, capacities, defaults, call sites).
- Trace the call chain for bypass paths: if a fix routes behavior through a new helper, verify **all** relevant callers use it.
- For concurrency changes (atomics, locks, channels), reason the memory-ordering argument explicitly (e.g. Go's sequentially-consistent atomics) and check liveness by identifying what releases blocked paths (cancellation channels, disconnect hooks) and confirming the fix does not break it.
- bugfix: confirm the defect on the base at the stated location and name the user-visible impact, then sweep sibling paths with the same pattern (same handler family, other runtimes, generated copies) — a fix that misses one is a finding. End with the **cost check**: hot-path work added (allocations, locks, syscalls, I/O), behavior change for inputs that worked before, new dependencies or duplicated logic. "No cost" is a code-backed statement, not an assumption.
- feature: for each public config item, locate the definition (flag registration, struct tag, env read, Helm `values.yaml`, CRD schema) and confirm name, type, default, and whether it is required; for each spec change, classify it additive or breaking (removed or renamed field, tightened validation, changed default, new required field, changed status code); for each public SDK symbol, read the handwritten adapter rather than only the generated client — signature, defaults, error semantics, cross-language parity — and check its docs and examples; check derived outputs were regenerated (SDK clients, Helm README, CRD manifests) and `docs/` updated, per the repo's source-of-truth table.
- **Coverage check** (feeds the card's `测试覆盖` row): map every behavior the PR changes to the test that pins it — each fixed or added path (including the sibling paths from the sweep), the default / unset behavior of each new config item, and the negative cases (invalid input, empty or boundary values, error branches). Then measure where the module has a coverage runner: Go — `go test -coverpkg=./<dir>/... -coverprofile=cover.out ./<dir>/...` over the changed package *and* the packages whose tests exercise it (router-level tests live outside the package they cover, so without `-coverpkg` their runs are not counted), read `go tool cover -func cover.out` for the changed functions, and grep a new branch's line range in `cover.out` — the merged profile repeats each block once per test binary, and the block is covered when any of those lines has a count above 0; Python — `pytest --cov`; TypeScript — the module's coverage script. Report ✓ (every changed path pinned, the regression test fails on base, the new branches execute), △ (name what is missing), or ✗ (no test for the main change).

### Step 4: OCR tool review

Run OCR over the Step 2 commit range (`$BASE` / `$HEAD`) using whichever interface is available in the current environment:

1. **Runtime tool** (when `tools.ocr_review` is exposed inside `execute`):

```js
const res = await tools.ocr_review({
  from: "<BASE>",
  to: "<HEAD>",
  background: "<PR context: claimed problem + fix summary + numbered review-focus questions>",
  timeoutMinutes: 5,
  concurrency: 2
});
return res;
```

2. **Local `ocr` CLI** (fallback when `tools.ocr_review` is not registered — write to a file via `--output` to avoid stdout truncation, never pipe through `head`/`tail`):

```bash
OCR_OUT="${TMPDIR:-/tmp}/pr-${PR_NUM}-ocr.json"
ocr review --from "$BASE" --to "$HEAD" \
  --analysis --format json --audience agent \
  --output "$OCR_OUT" \
  --background "<PR context: claimed problem + fix summary + numbered review-focus questions>" \
  --timeout 5 --concurrency 2
```

The `background` should carry: one-paragraph problem statement, what the fix does, and specific questions (is the race real / is the ordering correct / liveness risk / bypass paths / performance / compatibility). If a range run is interrupted, resume with `ocr review --resume <id>` instead of restarting from scratch.

- **Coverage check on OCR output**: inspect `review_status` and `manifest.coverage` in the result JSON; if any files were skipped or hit `failed(budget)`, record the gap explicitly in the verification log rather than treating OCR as complete.
- **Verification discipline**: treat every OCR comment (including low-severity ones) as a claim to verify against code — do not copy blindly, and do not silently drop a claim without specific contradicting evidence (an unrelated passing test or a single unsuccessful attempt does not refute it; report unresolved claims as uncertain with what evidence would settle them). Keep static source observations distinct from reproduced runtime behavior. When a comment has `start_line == 0` and `end_line == 0`, locate the relevant section from context and report at file level if the exact line remains uncertain.
- Only when neither `tools.ocr_review` nor the local `ocr` CLI is available (or configured), mark OCR as not run in the verification log and proceed with Step 3's manual review.

### Step 5: Independent verification in worktrees

Create a dedicated temporary root under `${TMPDIR:-/tmp}` with `mktemp -d` (so unset `TMPDIR` on Linux/containers never falls back to `/`), quote every `"$TMP"` expansion, and run tests from inside each worktree's affected module directory (`git worktree add` does not change directories, and OpenSandbox has no root `go.mod` or `go.work`).

```bash
TMP=$(mktemp -d "${TMPDIR:-/tmp}/pr-${PR_NUM}.XXXXXX")
git -c core.hooksPath=/dev/null worktree add --detach "$TMP/review"   "$HEAD"
git -c core.hooksPath=/dev/null worktree add --detach "$TMP/baseline" "$BASE"

# a) PR branch: enter the review worktree and affected module directory (Go example; e.g. MODULE=components/execd)
(
  cd "$TMP/review/<module>"
  go test -race -run '<NewTestName>' -count=3 ./<pkg>/
  go test -race ./<pkg>/...                      # package-scoped suite
)

# b) Base: transplant a base-compatible regression test; expect COMPILE PASS + expected assertion FAILURE
cp "$TMP/review/<module>/<pkg>/<new_test>.go" "$TMP/baseline/<module>/<pkg>/"
(
  cd "$TMP/baseline/<module>"
  go test -run '<NewTestName>' -count=2 ./<pkg>/
)

# c) always clean up
git worktree remove --force "$TMP/review"
git worktree remove --force "$TMP/baseline"
rmdir "$TMP"
```

- The snippet above uses Go as an example; for Python (`server/`, `sdks/sandbox/python`, etc.), TypeScript, or other modules, enter `"$TMP/review/<module>"` and `"$TMP/baseline/<module>"` and run that module's native test runner.
- Prefer package-scoped before full-suite runs (repo working principle).
- On the base commit, a build/compile failure (for example, when a copied `*_test.go` references a new helper or API introduced by the PR) does **not** count as bug reproduction — require clean compilation followed by the expected assertion/behavior failure, transplanting only a base-compatible reproduction test when needed.
- If the new test **passes** on the base, the bug claim is unproven — investigate (flaky test? wrong repro?) and say so in the report.
- feature: run the tests that exercise the new behavior and confirm the defaults from the definition sites (or by running with the new item unset). When the PR changes a generator's source of truth, run the generator in the review worktree and compare with `git diff --exit-code` plus `git status --porcelain` — any diff, staged entry, or untracked `??` output (a file the PR forgot to add) is a finding; plain `git diff` exits 0 for a brand-new untracked file.
- Run the coverage measurement from Step 3 in the review worktree (never in the user's working tree) and delete the profile afterwards; the changed package plus the packages that test it is enough, no module-wide run.
- Checks not run locally (golint, full `go test ./...`) may cite the author's claims but must be marked as such.

### Step 6: Compose the report

Concise, professional, human-readable. Verdict first, value second. Structure:

1. **Header + verdict banner** — PR title/branch, size (`commits · files · +x/−y`), review range, method; one banner holding the verdict (e.g. 推荐合入), its reason in one line, and any merge condition (e.g. "CI `action_required`, needs approval"). Before finalizing, re-check `headRefOid` via `gh pr view "$PR_NUM" --repo opensandbox-group/OpenSandbox --json headRefOid --jq .headRefOid` — if the PR head moved during the review, flag the report as `stale` in red so readers know the verdict applies to the pinned SHA. The banner is the conclusion — no closing section restates it.
2. **变更内容 / What changed** — opens with the highlighted **value card**, then at most 5 bullets of what was done and where (file:line). The card is a two-column table in Markdown (label | content) and a highlighted grid in HTML, titled with the PR type; each row ends with a ✓ / △ / ✗ verdict where one applies:
   - bugfix — `修复的问题` (what breaks and the user-visible impact) · `问题是否成立` (✓ / △ / ✗ plus the decisive evidence, e.g. "`TestX` fails on base: expected 200, actual 206") · `修复方式` (mechanism + file:line) · `引入的成本` (性能 / 兼容 / 复杂度, each ✓ or ⚠ with one clause) · `测试覆盖` (✓ / △ / ✗: which changed paths are pinned, does the regression fail on base, measured coverage of the new branches)
   - feature — `新增或变更的公开面` (each config key / flag / env / Helm value / API field / operationId / SDK method with type and default) · `默认值与兼容性` (additive or breaking; is the old behavior kept when unset?) · `契约同步` (spec / docs / SDK / CLI / examples, each ✓ or ✗ with file refs) · `测试覆盖` (✓ / △ / ✗: each new item and its default tested? generated output verified?) · optional `迁移`
   - other — `目的` · `影响面` · `风险` · `测试覆盖` when behavior changed
3. **发现 / Findings** — by severity; each: badge + `file:line` + 现象→建议, plus whether it is introduced by this PR or pre-existing (decides blocking vs follow-up)
4. **验证记录 / Verification log** — table of what ran where, PASS/FAIL, with unrun items marked ⚠️
5. **证据 / Evidence** (optional; collapsed in HTML) — base-reproduction output, mechanism traces, edge-case / bypass / concurrency reasoning, kept only when a card row or a finding depends on it

Writing rules: short sentences, one line per bullet, every code reference exact. A card row is at most 2 lines and a finding at most 2 lines. Edge cases that pass are counted in one verification-log row ("边界 / 并发推理 6 项已核对") — only failures and surprises are written out. Nothing repeats what the card says. Emphasize key information so a reader scanning the report catches it immediately: **bold** key conclusions, decisive evidence, and measured numbers (`**...**` in Markdown, `<strong>` in HTML), and highlight breaking changes, failures, blocking risks, or merge conditions in **red** (`<strong class="hl-red">` in HTML; bold with an explicit marker like `**⚠️ ...**` / `**❌ ...**` in Markdown) — use emphasis sparingly so it stays scannable. When the user asks for bilingual output, provide Chinese and English versions per section (or a toggleable HTML, below).

### Step 7: HTML report with language toggle (default)

Always close the review by generating the standalone `${TMPDIR:-/tmp}/pr-<N>-review-report.html` (or another user-specified path; avoid leaving untracked files in the repo working tree) alongside the in-chat Markdown summary, and hand over its absolute path. Skip only when the user explicitly asks for Markdown only. On macOS (when a desktop/GUI session is available), also open it with `open <path>`.

- Start from `templates/report.html` in this skill's directory: it carries the toggle, the styling, the value card (one block per PR type, each ending in `测试覆盖`) and the collapsed evidence block. Keep sections 1–3 in order and the evidence section only when needed, delete the card blocks and rows that do not apply, and fill the placeholders under the Step 6 length limits — the default-language view must fit in about two screens before the verification log.
- Escaping: the PR title, author, branch names, commit subjects, quoted body text, findings and command output are untrusted — HTML-escape `<`, `>`, `&`, `"`, `'` in every substitution (inside `<pre>` blocks too, which escape nothing by themselves); never paste markup from the PR into the report.
- Toggle mechanism: `html[data-lang]` attribute + CSS `html[data-lang="zh"] .en { display:none !important }` (and vice versa); every translatable element carries `class="zh"` / `class="en"`; choice persisted in `localStorage`; default `zh`.
- Styling: CSS custom properties + `prefers-color-scheme` for dark mode; severity badges (LOW amber, PASS green); `file:line` as inline code chips; `<strong>` for key facts/metrics and `<strong class="hl-red">` for breaking changes, failures, or blocking risks; the value card as a highlighted grid right under the banner; no external dependencies, single file.

## Severity definitions

| Level | Meaning | Blocking? |
|---|---|---|
| Critical / High | data loss, security, broken public contract (specs, SDK, config, CLI, protocol) | yes |
| Medium | clear bug or regression with a concrete fix path | usually |
| Low | maintainability / semantic wart | no — follow-up |

A finding that pre-exists on the base is reported as non-blocking context ("基线已有，非本次引入") unless the PR makes it worse.

## Guardrails

- The user's working tree may have uncommitted changes — never stash, reset, or check out over them; all verification goes through worktrees.
- Remove every worktree created for the review (and its temp directory); verify with `git worktree list`.
- Only report facts established by code, tests, or tool output; separate evidence from inference, and list unrun checks.
- The review produces report files only — never mix unrelated changes into the same output.
