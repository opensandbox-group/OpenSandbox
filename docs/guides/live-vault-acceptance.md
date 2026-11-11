---
title: Live Vault Acceptance (Experimental)
description: Task spec, plan and acceptance evidence matrix for the OSEP-0023 experimental live credential-bound Vault loop in the egress sidecar.
---

# OSEP-0023 Live Credential-Bound Vault: Acceptance

::: warning Internal task document
This page separates the current Policy revision acceptance from the historical live-Vault evidence that preceded PR #2176. Earlier `fresh` or `pass` results do not validate the Policy integration or establish a passing current CI run. The runtime remains default-off and unsupported as a public deployment mode.
:::

## Policy revision integration acceptance

- Branch: `feat/osep0023-policy-revision`.
- Implementation starting commit: `1adfcecec408c2732071befb16f2412463bc3315`.
- Design baseline: `12b77ddefdeb7b35cd933bad18cf253e2c02fcbf` (PR #2176 merge).
- Scope: existing Policy API handlers and automatic always-rule reloads join the shared Policy/Vault mutation barrier. Subsequent Vault updates and bootstrap retain the committed internal Policy epoch. No public methods, schema fields, supported mode, HTTP/2 or Fast Sandbox support are added.

Success requires optional user-policy file persistence, nft application, exact receiver confirmation, then Go proxy/base publication. Reload omits user-policy persistence. This is not atomic pre-ACK visibility: nft can affect traffic first. Receiver failure after successful nft requires sticky recovery and quarantine, even on definitive rejection. API success waits for publication and old-request deadline registration, not the default 30-second drain. Expiry closes the whole connection, potentially affecting newer requests; repeated revisions do not extend the earliest old-request deadline.

The following rows separate executed checks from blocked acceptance layers. Selected unit-test passes are not full-suite or end-to-end acceptance. Command summaries below apply to the current implementation worktree; the final commit identity belongs in the corresponding validation handoff.

| ID | Required evidence | Current result |
|---|---|---|
| P1 | Go owner: Policy methods/reset, no-ops, binding preflight, exact candidate/identity, file/nft failures, cancellation, serialized reload and lifecycle cleanup | Fresh pass: selected owner units and focused race checks, including authoritative Policy shadowing and all three shutdown paths. Broader permitted selection: 18 packages, 594 top-level passes; exclusions and skips detailed below |
| P2 | Real Go → Unix → Python: Policy and reload confirmation, lost-ACK reconciliation, Policy → Vault epoch continuity, bootstrap fencing | Blocked: AF_UNIX socket creation returns EPERM in this environment |
| P3 | Real mitmdump: policy-only keepalive, still-covered idle reuse, long request/upgrade retirement, earliest deadline and whole-connection closure | Registry units: 69/69 pass. Real runtime blocked: AF_UNIX EPERM causes 14 setup failures; one negative startup check passes, not an aggregate runtime pass |
| P4 | Privileged Linux file/nft/quarantine: unchanged start failure, committed effect followed by receiver rejection/unknown, actual packet fence | Blocked: nft and required capabilities are unavailable; unshare returns EPERM |
| P5 | Current-checkout image: Policy methods, bound-deny rejection, real periodic always reload, default 30-second drain, Policy then Vault rotation; record image identity and owned-resource cleanup | Runner units: 33/33 pass. Image run fails prerequisite checks (exit 1, zero skips): Docker/nft absent and unshare returns EPERM |
| P6 | Existing Go SDK PatchPolicy/DeletePolicy 200 structure and 503 propagation; narrow spec generation; docs-site build | Fresh pass: Go SDK full suite and focused Policy cases; docs-site build. Python/TypeScript generation is limited to the documented responses; both Python handlers pass 200/503 response checks and the generated TypeScript contracts pass strict type checking |

Current checks (2026-10-11 UTC) ran on the starting commit plus the Policy integration worktree:

- Egress Go tests used package-scoped `go test -json` selections with 64 named Unix-socket or privileged tests explicitly excluded. The permitted selection passes in 18 packages, with 594 top-level test passes, four privileged tests skipped and two ACL subtests skipped. This is **not a full egress suite pass**.
- Selected owner units and `go test -race` checks pass, including the authoritative Policy shadowing regression and Policy API, always-rule reload and Vault shutdown paths. The broader focused race selection passes 75 top-level tests with zero skips or failures; it does not cover blocked IPC or privileged paths.
- `go vet ./...` and `go build .` pass with `GOFLAGS=-buildvcs=false`, because the environment cannot execute the VCS-stamping subprocess. Go dependency checks use the provisioned module cache with `GOPROXY=off` and `GOSUMDB=off`.
- Python discovery finds 363 tests; 53 runtime/Unix-socket cases are explicitly excluded from the pure-Python selection. All 310 selected tests pass with zero skips, including the 69 registry tests. This is **not full discovery acceptance** and does not replace real mitmdump evidence.
- `python -m unittest discover -v -s scripts -p 'test_*.py'` under `components/egress` passes 85 tests, including the 33 image-runner unit tests.
- `cd sdks/sandbox/go && go test ./...`: pass; main SDK package and the `poolredis` package (which has no tests).
- `go test -v -run 'Test(PatchPolicy|DeletePolicy|PolicyMutationServiceUnavailable)$'`: pass, including separate PATCH and DELETE 503 cases.
- Egress-only regeneration with `openapi-python-client 0.28.0` and `openapi-typescript 7.13.0`: baseline outputs match the checked-in files; updated outputs change only two Python Policy handlers and the TypeScript Policy response types/descriptions. Public schemas and methods are unchanged.
- Generated Python `sync_detailed` handlers: mocked HTTP 200 and plain-text 503 responses pass for PATCH and DELETE, including strict unexpected-status mode.
- TypeScript 5.9.3: strict no-emit check of the generated Policy 200/503 contracts passes. This is not a full Python or JavaScript SDK suite result.
- `cd docs && pnpm docs:build`: pass under Node 22.23.3 and pnpm 9.15.0; VitePress reports a bundle-size warning, with zero build errors.

The image runner includes `TestPolicyRevisionAPIMutations`, `TestPolicyRevisionRejectsBoundDeny`, `TestAlwaysReloadRevision`, `TestPolicyOnlyDrain` and `TestPolicyThenVaultMutation`. The Policy drain case uses the production 30-second default without an override; always-rule cases wait for the actual one-minute reload job. Their presence and runner-unit coverage do not establish image acceptance when prerequisites fail. No image was built and no real-image case ran in this environment. Real IPC, mitmdump, nft/packet and image acceptance remain blocked; no full acceptance or current GitHub CI pass is claimed.

Neither unit tests nor loopback runtime tests replace image/packet evidence. Missing Docker, nft, namespace permissions or dependencies must be reported as blocked rather than a passing skip. Image evidence must identify the actual commit/tree, dirty-diff fingerprint, image ID, executed cases, return codes and cleanup results. Do not expose credentials or add a public epoch debug API.

## Historical live-Vault evidence (PR #2176)

The remaining record is retained from the earlier live-Vault work. Its branch, baselines, session-relative evidence states and deferred checks belong to that work and are not the status of the Policy revision integration above.

- Branch: `feat/osep0023-live-vault`
- Validation baseline: `f46ff91ef572589daeeb67b453ea4e8714a68a26` was the clean starting commit. Local checks ran against that baseline plus the repair worktree, not against the unchanged baseline alone.
- Retained implementation base: `447a7be3bd3c0ee0ac2e9da5412c281aa44b1ca9`. This publication does not rebase onto upstream main; PR creation and base selection remain with the user.
- Owner: current Devin session (implementation + focused tests). Independent review is unavailable in this context — **blocked**; lead review of the full diff is complete and does not count as independent.
- Write set: `components/egress/**`, `docs/guides/credential-vault.md`, `docs/guides/live-vault-acceptance.md`, `components/egress/docs/live-vault-acceptance.md`, `oseps/0023-credential-bound-tls-interception.md`.
- Historical evidence notes: earlier runs reported "18 Go packages ok", "309 Python tests, 0 skips", "6/6 image cases PASS" — these are self-reported and unverified; no current pass is claimed until fresh logs land in the evidence directory for this session.

### Task spec

Harden the experimental live credential-bound loop before it can be trusted:

1. Unknown mutation outcomes must latch sticky recovery instead of restarting from possibly-stale prior state (AC1).
2. `ssl_insecure` must be refused wherever the live admission bundle is active (AC2).
3. ECH-bearing ClientHellos must be detected from the actual extension list and passed through opaquely; uninspectable data denies (AC3).
4. Decrypted HTTP/1 requests must prove their authority equals the admitted SNI before any credential can be injected (AC4).
5. The real Vault lifecycle (rotation, keepalive, stale revision, remove/re-add) holds under the real runtime (AC5).
6. Drain must page all expired transports, retry failed closes, and clean up all runner-created resources with recorded results (AC6).
7. Legacy installation-only behavior is unchanged (AC7).

### Plan

- Go: latch `requireRevisionRecoveryLocked(ExternalEffectsUnknown)` before every terminal detach in `mutateRevisionVault`; reject `LiveAdmission && ssl_insecure` in `validateRevisionIPCConfig`.
- Python addon: ECH detection from `ClientHello.extensions`, strict H1 authority validation, ssl_insecure fences in load/configure/per-request, cursor-based drain paging.
- Harness: live-vault image runner records per-resource cleanup results and a sanitized evidence schema; real-mitmproxy and raw-wire tests provide the boundary evidence.
- Verification: focused offline Go tests, focused Python unit tests, real loopback mitmdump tests; Docker image tests only after explicit authorization.

### Acceptance criteria and evidence matrix

Evidence state is tracked per AC: `fresh` = produced by this session's runs, `reused` = pre-existing unchanged coverage, `missing` = not yet run, `blocked` = needs a permission or resource this context lacks. Pass/fail is recorded only for checks that actually executed in this session.

| AC | Requirement | Evidence layers | Evidence state | Result |
|----|-------------|-----------------|----------------|--------|
| AC1 | Uncertain lifecycle: every terminal detach latches recovery; fresh bootstrap, stale tickets and watcher restarts rejected; old credentials never reauthorized; known rejection/cancelled/shutdown paths nonsticky | Go unit + Python-IPC integration tests (`TestRevision*`) | fresh | pass — `go test ./...` exit 0, 18 packages (log `rework-20261010/go-test-all.txt`); Go test-case skip count not recorded (not verbose) |
| AC2 | Cert/security config: live bundle + `ssl_insecure` rejected in Go launch validation and by real mitmdump startup; safe setting and legacy accepted; lazy handshake verifies upstream | Go env tests, real mitmdump startup tests | fresh | pass — 3/3 `LiveSslInsecureStartupTest` under real mitmdump |
| AC3 | ECH opaque: extension 0xfe0d (incl. GREASE) detected from the real extension list; opaque ignore_connection, no token, zero credential visibility; malformed data denies | subprocess-isolated raw-wire tests parsed by real `mitmproxy.tls.ClientHello` + real-mitmdump boundary test | fresh | pass — 2/2 `test_ech_wire` incl. real-mitmdump origin-vs-mitm cert contrast |
| AC4 | H1 authority: exactly one strict Host equal to SNI and admission token; absolute target agreement; CONNECT/dup/missing/malformed/H2 denied before injection | unit + subprocess-isolated real `read_request_head` tests | fresh (unit/parser); deferred (Docker raw H1 inputs, image layer) | pass — 17 real-parser worker cases; image layer deferred |
| AC5 | Real Vault lifecycle: rotation on keepalive, stale expectedRevision conflict, removal fence, remove/re-add | real mitmdump runtime tests; Docker image run | fresh (loopback); deferred (image run, user decision) | pass (loopback) — 344 Python tests incl. 6 runtime cases, 0 skips; Vault HTTP API stale/remove-readd in real Docker remains deferred |
| AC6 | Idle/multipage drain: after_serial paging, failed close retried, no starvation, no deadline extension, full cleanup accounting | unit tests + real mitmdump idle-close loopback test + runner unit tests; image idle-drain case | fresh (unit+loopback); deferred (image layer) | pass — idle transport closes only at deadline; runner mocked-cleanup units pass |
| AC7 | Legacy unchanged: installation-only receiver, pull-based vault path and non-live envs keep existing behavior | existing tests kept green | fresh | pass — included in the 344-test discovery |

The Docker image layer (real transparent port-443 interception, container lifecycle) is **deferred by user decision**: image acceptance is deferred to GitHub CI or a later session. It has not run, no CI has passed, and this deferral is not an acceptance downgrade — the code is prepared so the image suite can run unchanged once authorized. The docs-site build also remains deferred (offline dependency store incomplete).

### Evidence layers

Unit tests do not substitute for the real-runtime run, and no environment skip is reported as a pass. Fresh per-command logs for this session are written under the session evidence directory (`/tmp/live-vault-evidence-final/`), one child directory per run, and are linked from the handoff report rather than copied into this page.

- Go unit and cross-language IPC integration: `cd components/egress && umask 022 && GOPROXY=off GOSUMDB=off go test . -run 'TestRevision(VaultMutation|WriteHandler|Recovery)'` and `go test ./pkg/mitmproxy ./pkg/revisionruntime`.
- Python addon unit tests: `python -m unittest discover -v -s tests -p "test_*.py"` under the provisioned mitmproxy 11.0.2 venv.
- Real-mitmdump runtime tests (loopback only, disposable): `test_credential_bound_runtime.py`, `test_mitmproxy_runtime.py`, ECH/H1 wire tests.
- Real Docker image end-to-end: `components/egress/scripts/test_live_vault_image.py --image <egress image> --artifacts <evidence dir>` — **deferred by user decision** (to GitHub CI or a later session); not run in this session.

### Scope exclusions

- HTTP/2 GOAWAY/stream drain, the fast-sandbox subject profile and recovery across a complete sidecar replacement remain out of scope. Dynamic Policy and always-rule reload participation were excluded from this historical slice; they have a separate acceptance matrix above.
- Dynamic DNS learning and Kubernetes deployments are not verified by this experiment's current evidence; the Docker-network runner pins origin addresses and uses static allow rules (the local Docker embedded DNS is a runner convenience, unrelated to runtime DNS or Kubernetes evidence).
- The Dockerfile build knobs `GOPROXY` and `APT_MIRROR` remain optional and empty by default; they are local-mirror conveniences for restricted build networks only. Override setup errors fail the build; `APT_MIRROR` also requires at least one supported APT source file.

### Evidence hygiene

Test output and this document contain no credential values: fixtures mask injected headers, the origin records only header names, the runner sanitizes known fixture secrets in recorded commands, and runtime failures are reported through fixed error classes.

### Commit Identity Rewrite

The user authorized rewriting only the four live-Vault slice commits. Their author and committer are now `hpliStartAgain` with the verified-account noreply address `118508408+hpliStartAgain@users.noreply.github.com`. Each commit retains its original tree, timestamps, and Claude co-author trailer. The evidence commit's message now references the rewritten implementation commit.

| Original SHA | Rewritten SHA |
|---|---|
| `a90a41330029fb646be5c63de86b3d4d8f4ac40a` | `362e7136f14621e5015ab7447b9fb7eff19feeae` |
| `a6354dea8f8df4c523fa68b146e1b8d2e4df9341` | `6c6619d38b00b6672680ac4ec69a430c7959e1a9` |
| `205d6c8601997415a34b3159b6a7e027dffe8559` | `87a3133b94202480d737057bc3043684ab24c57a` |
| `f46ff91ef572589daeeb67b453ea4e8714a68a26` | `437ff10c43cf830bd3cf91e762f33b78cd1a45ad` |

The old baseline references identify the original validation history. The security repair is a separate child commit, not part of this metadata-only rewrite. A local backup ref retains the old published tip.
