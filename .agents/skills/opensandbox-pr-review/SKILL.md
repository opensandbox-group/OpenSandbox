---
name: opensandbox-pr-review
version: 1.0.0
description: Deep-review a GitHub PR in the OpenSandbox repository (评审 PR / review PR / 给我一份评审报告 / 中文报告 / 中英双语报告). Runs OCR tool review on the PR ref range, independently verifies every factual claim in code and tests (bug reproduction on the base commit, tests on the PR branch inside isolated worktrees), and produces a concise professional report — Markdown in chat, optionally a standalone HTML with Chinese/English toggle — with severity-graded findings and exact file:line references.
---

# OpenSandbox PR Review (OCR + Manual Verification)

Deep review workflow for a GitHub PR against this repository: tool-assisted OCR review + independent manual verification + reproducible evidence + concise bilingual report. Reference example: PR #2979 (execd reply deadline fix).

## Input

| Item | Example | Required |
|---|---|---|
| PR URL or number | `https://github.com/opensandbox-group/OpenSandbox/pull/2979` | yes |
| Review focus / business context | "提出的问题是否成立，性能与兼容性影响" | no (default: full review) |
| Output requirements | 中文报告 / 中英双语 / HTML 可切换 | no (default: Chinese Markdown) |

## Principles

1. **Never trust the PR description.** Every factual claim (file:line, constant value, channel capacity, default timeout, enum) is verified at its definition site in code. Claims that cannot be resolved are dropped or marked as unverified — never paraphrased as fact.
2. **Reproduction first.** For a bug-fix PR, the new regression test must be run against the base commit and shown to fail there. A fix for a bug that cannot be reproduced is a red flag, reported explicitly.
3. **Isolated verification.** All test runs happen in detached worktrees. Never touch the user's working tree (it may carry uncommitted changes); never check out the PR branch in place.
4. **Dual-channel review.** OCR tool review and own reading of the diff + full changed files are complementary. Both run; conclusions merge.
5. **Report discipline.** Verdict first, exact `file:line` references, findings graded by severity, a verification log table, and explicit annotation for checks not run locally.

## Workflow

### Step 1: Gather PR metadata

Fetch PR metadata with `gh` (structured JSON — no HTML scraping):

```bash
gh pr view <number> --repo opensandbox-group/OpenSandbox \
  --json title,body,author,baseRefName,headRefName,headRepositoryOwner,commits,files,labels
```

Extract from the output:

- Title, base branch, head branch (plus fork owner), author, commit list
- The claimed problem: mechanism, reproduction claim, stated fix, stated testing, breaking-change statement
- Build a **claim checklist** from the body — every concrete assertion to verify in later steps (e.g. "resultChan holds 10 results", "grace period 2s by default", "fails on main with expected 15, actual 11")

### Step 2: Fetch refs locally

```bash
# PR author remote (reuse if it already exists — check git remote -v first)
git remote get-url pr-<author> 2>/dev/null || \
  git remote add pr-<author> git@github.com:<author>/OpenSandbox.git
git fetch pr-<author> <head-branch>
git fetch origin main

BASE=$(git merge-base origin/main pr-<author>/<head-branch>)
git log --oneline $BASE..pr-<author>/<head-branch>
git diff --stat $BASE..pr-<author>/<head-branch>
git diff $BASE..pr-<author>/<head-branch>
```

Record `<base>..<head>` — this range feeds both manual reading and OCR.

### Step 3: Manual verification of code

- `Read` the **full changed files**, not just the diff hunks. Line numbers in the report must reference the PR-head version.
- For each item in the claim checklist, locate the definition with `grep`/`read` and confirm or refute it (constants, capacities, defaults, call sites).
- Trace the call chain for bypass paths: if a fix routes behavior through a new helper, verify **all** relevant callers use it.
- For concurrency changes (atomics, locks, channels), reason the memory-ordering argument explicitly; confirm the guarantee holds under Go's sequentially-consistent atomics.
- Check liveness: identify what releases blocked paths (cancellation channels, disconnect hooks) and confirm the fix does not change it.

### Step 4: OCR tool review

Run inside the `execute` tool, with `from`/`to` set to the Step 2 range:

```js
const res = await tools.ocr_review({
  from: "<merge-base>",
  to: "pr-<author>/<head-branch>",
  background: "<PR context: claimed problem + fix summary + numbered review-focus questions>",
  timeoutMinutes: 5,
  concurrency: 2
});
return res;
```

The `background` should carry: one-paragraph problem statement, what the fix does, and specific questions (is the race real / is the ordering correct / liveness risk / bypass paths / performance / compatibility). The result JSON provides findings with severity, path, line numbers, and suggestions — merge them with Step 3 conclusions; do not copy blindly.

### Step 5: Independent verification in worktrees

```bash
TMP=$TMPDIR  # or the approved session temp dir
git worktree add --detach $TMP/pr-<N>-review   pr-<author>/<head-branch>
git worktree add --detach $TMP/pr-<N>-baseline <merge-base>

# a) PR branch: new/changed tests, repeated, race-enabled (Go)
go test -race -run '<NewTestName>' -count=3 ./<pkg>/
go test -race ./<pkg>/...                      # package-scoped suite

# b) Base: copy new test files over, expect FAILURE (bug reproduction)
cp $TMP/pr-<N>-review/<pkg>/<new_test>.go $TMP/pr-<N>-baseline/<pkg>/
cd $TMP/pr-<N>-baseline && go test -run '<NewTestName>' -count=2 ./<pkg>/

# c) always clean up
git worktree remove --force $TMP/pr-<N>-review
git worktree remove --force $TMP/pr-<N>-baseline
```

- Prefer package-scoped before full-suite runs (repo working principle).
- If the new test **passes** on the base, the bug claim is unproven — investigate (flaky test? wrong repro?) and say so in the report.
- Checks not run locally (golint, full `go test ./...`) may cite the author's claims but must be marked as such.

### Step 6: Compose the report

Concise, professional, human-readable. Verdict first. Structure:

1. **Header** — PR title/branch, size (`commits · files · +x/−y`), review range, method; a one-line verdict banner (e.g. 推荐合入 + reason)
2. **变更内容 / What changed** — 3–5 bullets, what was done and where (file:line)
3. **缺陷验证 / Bug verification** — mechanism (verified), reproduction result, concurrency-correctness reasoning, liveness — each one bullet
4. **评估 / Assessment** — verdict-style table: performance / compatibility / tests, one line each with a ✓ verdict
5. **发现 / Findings** — by severity; each finding: badge + `file:line` + 现象→建议, plus whether it is introduced by this PR or pre-existing (decides blocking vs follow-up)
6. **验证记录 / Verification log** — table of what ran where, PASS/FAIL, with unrun items marked ⚠️
7. **结论 / Conclusion** — one short paragraph

Writing rules: short sentences, one line per bullet, no repeated argumentation, every code reference exact. When the user asks for bilingual output, provide Chinese and English versions per section (or a toggleable HTML, below).

### Step 7 (optional): HTML report with language toggle

When asked for an HTML version (or 双语切换报告), generate a standalone `pr-<N>-review-report.html` at the repo root:

- Toggle mechanism: `html[data-lang]` attribute + CSS `html[data-lang="zh"] .en { display:none !important }` (and vice versa); every translatable element carries `class="zh"` / `class="en"`; choice persisted in `localStorage`; default `zh`.
- Styling: CSS custom properties + `prefers-color-scheme` for dark mode; severity badges (LOW amber, PASS green); `file:line` as inline code chips; no external dependencies, single file.
- Keep it as lean as the Markdown version — same 6-section structure, verdict banner up top.
- Deliver the **absolute path**; open with `open <path>` on macOS when the user asks.

## Severity definitions

| Level | Meaning | Blocking? |
|---|---|---|
| Critical / High | data loss, security, broken public contract (specs, SDK, config, CLI, protocol) | yes |
| Medium | clear bug or regression with a concrete fix path | usually |
| Low | maintainability / semantic wart | no — follow-up |

A finding that pre-exists on the base is reported as non-blocking context ("基线已有，非本次引入") unless the PR makes it worse.

## Guardrails

- The user's working tree may have uncommitted changes — never stash, reset, or check out over them; all verification goes through worktrees.
- Remove every worktree created for the review; verify with `git worktree list`.
- Only report facts established by code, tests, or tool output; separate evidence from inference, and list unrun checks.
- The review produces report files only — never mix unrelated changes into the same output.
