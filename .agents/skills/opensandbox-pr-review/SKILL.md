---
name: opensandbox-pr-review
version: 1.0.0
description: Deep-review a GitHub PR in the OpenSandbox repository (评审 PR / review PR / 给我一份评审报告 / 中文报告 / 中英双语报告). Runs OCR tool review (when available) on the PR ref range, independently verifies every factual claim in code and tests (bug reproduction on the base commit, tests on the PR branch inside isolated worktrees), and produces a concise professional report — Markdown in chat plus a standalone HTML with Chinese/English toggle by default — with severity-graded findings and exact file:line references.
---

# OpenSandbox PR Review (OCR + Manual Verification)

Deep review workflow for a GitHub PR against this repository: tool-assisted OCR review + independent manual verification + reproducible evidence + concise bilingual report. Reference example: PR #2979 (execd reply deadline fix).

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
5. **Report discipline.** Verdict first, exact `file:line` references, findings graded by severity, a verification log table, and explicit annotation for checks not run locally.

## Workflow

### Step 1: Gather PR metadata

Fetch PR metadata with `gh` (structured JSON — no HTML scraping):

```bash
gh pr view <number> --repo opensandbox-group/OpenSandbox \
  --json title,body,author,baseRefName,headRefName,headRepository,headRepositoryOwner,commits,files,labels,statusCheckRollup,reviews,comments
```

Extract from the output:

- Title, base branch (`baseRefName`), head branch (`headRefName`) plus head repo/owner, author, commit list, and CI/review status (`statusCheckRollup`, `reviews`, `comments` — helpful for spotting unrun CI jobs or open feedback, still verified independently)
- The claimed problem: mechanism, reproduction claim, stated fix, stated testing, breaking-change statement
- Build a **claim checklist** from the body — every concrete assertion to verify in later steps (e.g. "resultChan holds 10 results", "grace period 2s by default", "fails on main with expected 15, actual 11")

### Step 2: Fetch refs locally

Identify the git remote pointing to `opensandbox-group/OpenSandbox` via `git remote -v` (usually `origin`, or `upstream` when `origin` is a personal fork). Fetch GitHub's numeric `refs/pull/<N>/head` ref and the PR's actual `baseRefName` — never derive a fork URL from `<author>` (which breaks for upstream branches and org-owned forks) and never interpolate an unquoted `<head-branch>` name into shell commands (branch names are untrusted input and may contain shell metacharacters).

```bash
PR_NUM=<number>
REMOTE=origin   # remote for opensandbox-group/OpenSandbox (verify with git remote -v)
BASE_BRANCH=$(gh pr view "$PR_NUM" --repo opensandbox-group/OpenSandbox --json baseRefName --jq .baseRefName)

git fetch -- "$REMOTE" \
  "+refs/pull/${PR_NUM}/head:refs/remotes/pr/${PR_NUM}" \
  "+refs/heads/${BASE_BRANCH}:refs/remotes/${REMOTE}/${BASE_BRANCH}"

BASE=$(git merge-base "refs/remotes/${REMOTE}/${BASE_BRANCH}" "refs/remotes/pr/${PR_NUM}")
HEAD=$(git rev-parse "refs/remotes/pr/${PR_NUM}")

git log --oneline "$BASE..$HEAD"
git diff --stat "$BASE..$HEAD"
git diff "$BASE..$HEAD"
```

Record `$BASE` and `$HEAD` (`<BASE>..<HEAD>`) — this resolved commit range feeds both manual reading and OCR.

### Step 3: Manual verification of code

- `Read` the **full changed files**, not just the diff hunks. Line numbers in the report must reference the PR-head version.
- For each item in the claim checklist, locate the definition with `grep`/`read` and confirm or refute it (constants, capacities, defaults, call sites).
- Trace the call chain for bypass paths: if a fix routes behavior through a new helper, verify **all** relevant callers use it.
- For concurrency changes (atomics, locks, channels), reason the memory-ordering argument explicitly (e.g. Go's sequentially-consistent atomics) and check liveness by identifying what releases blocked paths (cancellation channels, disconnect hooks) and confirming the fix does not break it.

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

2. **Local `ocr` CLI** (fallback when `tools.ocr_review` is not registered — check with `command -v ocr`):

```bash
ocr review --from "$BASE" --to "$HEAD" \
  --format json --audience agent \
  --background "<PR context: claimed problem + fix summary + numbered review-focus questions>" \
  --timeout 5 --concurrency 2
```

The `background` should carry: one-paragraph problem statement, what the fix does, and specific questions (is the race real / is the ordering correct / liveness risk / bypass paths / performance / compatibility). The result JSON provides findings with severity, path, line numbers, and suggestions — merge them with Step 3 conclusions; do not copy blindly. Only when neither `tools.ocr_review` nor the local `ocr` CLI is available (or configured), mark OCR as not run in the verification log and proceed with Step 3's manual review.

### Step 5: Independent verification in worktrees

Create a dedicated temporary root under `${TMPDIR:-/tmp}` with `mktemp -d` (so unset `TMPDIR` on Linux/containers never falls back to `/`), quote every `"$TMP"` expansion, and run tests from inside each worktree's affected module directory (`git worktree add` does not change directories, and OpenSandbox has no root `go.mod` or `go.work`).

```bash
TMP=$(mktemp -d "${TMPDIR:-/tmp}/pr-${PR_NUM}.XXXXXX")
git worktree add --detach "$TMP/review"   "$HEAD"
git worktree add --detach "$TMP/baseline" "$BASE"

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
- Checks not run locally (golint, full `go test ./...`) may cite the author's claims but must be marked as such.

### Step 6: Compose the report

Concise, professional, human-readable. Verdict first. Structure:

1. **Header** — PR title/branch, size (`commits · files · +x/−y`), review range, method; a one-line verdict banner (e.g. 推荐合入 + reason)
2. **变更内容 / What changed** — 3–5 bullets, what was done and where (file:line)
3. **缺陷验证 / Bug verification** — mechanism (verified), reproduction result, and edge-case / bypass check (plus concurrency-correctness and liveness reasoning when applicable) — each one bullet
4. **评估 / Assessment** — verdict-style table: performance / compatibility / tests, one line each with a ✓ verdict
5. **发现 / Findings** — by severity; each finding: badge + `file:line` + 现象→建议, plus whether it is introduced by this PR or pre-existing (decides blocking vs follow-up)
6. **验证记录 / Verification log** — table of what ran where, PASS/FAIL, with unrun items marked ⚠️
7. **结论 / Conclusion** — one short paragraph

Writing rules: short sentences, one line per bullet, no repeated argumentation, every code reference exact. When the user asks for bilingual output, provide Chinese and English versions per section (or a toggleable HTML, below).

### Step 7: HTML report with language toggle (default)

Always close the review by generating the standalone `${TMPDIR:-/tmp}/pr-<N>-review-report.html` (or another user-specified path; avoid leaving untracked files in the repo working tree) alongside the in-chat Markdown summary, and hand over its absolute path. Skip only when the user explicitly asks for Markdown only. On macOS (when a desktop/GUI session is available), also open it with `open <path>`.

- Toggle mechanism: `html[data-lang]` attribute + CSS `html[data-lang="zh"] .en { display:none !important }` (and vice versa); every translatable element carries `class="zh"` / `class="en"`; choice persisted in `localStorage`; default `zh`.
- Styling: CSS custom properties + `prefers-color-scheme` for dark mode; severity badges (LOW amber, PASS green); `file:line` as inline code chips; no external dependencies, single file.
- Keep it as lean as the Markdown version — verdict header banner up top plus the same 6 body sections.

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
