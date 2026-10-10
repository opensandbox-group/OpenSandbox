---
title: Live Vault Acceptance (Experimental)
description: Task spec, plan and acceptance evidence matrix for the OSEP-0023 experimental live credential-bound Vault loop in the egress sidecar.
---

# OSEP-0023 Live Credential-Bound Vault: Acceptance

::: warning Internal task document
This page tracks the experimental implementation's spec, plan and evidence.
It is not a supported feature document. Local security repairs and focused
checks are complete; real-image acceptance remains deferred. This does not
claim a passing GitHub CI run or release readiness.
:::

- Branch: `feat/osep0023-live-vault`
- Validation baseline: `f46ff91ef572589daeeb67b453ea4e8714a68a26` was the
  clean starting commit. Local checks ran against that baseline plus the
  repair worktree, not against the unchanged baseline alone.
- Retained implementation base: `447a7be3bd3c0ee0ac2e9da5412c281aa44b1ca9`.
  This publication does not rebase onto upstream main; PR creation and base
  selection remain with the user.
- Owner: current Devin session (implementation + focused tests). Independent
  review is unavailable in this context — **blocked**; lead review of the full
  diff is complete and does not count as independent.
- Write set: `components/egress/**`, `docs/guides/credential-vault.md`,
  `docs/guides/live-vault-acceptance.md`,
  `components/egress/docs/live-vault-acceptance.md`,
  `oseps/0023-credential-bound-tls-interception.md`.
- Historical evidence notes: earlier runs reported "18 Go packages ok",
  "309 Python tests, 0 skips", "6/6 image cases PASS" — these are self-reported
  and unverified; no current pass is claimed until fresh logs land in the
  evidence directory for this session.

## Task spec

Harden the experimental live credential-bound loop before it can be trusted:

1. Unknown mutation outcomes must latch sticky recovery instead of restarting
   from possibly-stale prior state (AC1).
2. `ssl_insecure` must be refused wherever the live admission bundle is active
   (AC2).
3. ECH-bearing ClientHellos must be detected from the actual extension list
   and passed through opaquely; uninspectable data denies (AC3).
4. Decrypted HTTP/1 requests must prove their authority equals the admitted
   SNI before any credential can be injected (AC4).
5. The real Vault lifecycle (rotation, keepalive, stale revision,
   remove/re-add) holds under the real runtime (AC5).
6. Drain must page all expired transports, retry failed closes, and clean up
   all runner-created resources with recorded results (AC6).
7. Legacy installation-only behavior is unchanged (AC7).

## Plan

- Go: latch `requireRevisionRecoveryLocked(ExternalEffectsUnknown)` before
  every terminal detach in `mutateRevisionVault`; reject
  `LiveAdmission && ssl_insecure` in `validateRevisionIPCConfig`.
- Python addon: ECH detection from `ClientHello.extensions`, strict H1
  authority validation, ssl_insecure fences in load/configure/per-request,
  cursor-based drain paging.
- Harness: live-vault image runner records per-resource cleanup results and a
  sanitized evidence schema; real-mitmproxy and raw-wire tests provide the
  boundary evidence.
- Verification: focused offline Go tests, focused Python unit tests, real
  loopback mitmdump tests; Docker image tests only after explicit
  authorization.

## Acceptance criteria and evidence matrix

Evidence state is tracked per AC: `fresh` = produced by this session's runs,
`reused` = pre-existing unchanged coverage, `missing` = not yet run,
`blocked` = needs a permission or resource this context lacks. Pass/fail is
recorded only for checks that actually executed in this session.

| AC | Requirement | Evidence layers | Evidence state | Result |
|----|-------------|-----------------|----------------|--------|
| AC1 | Uncertain lifecycle: every terminal detach latches recovery; fresh bootstrap, stale tickets and watcher restarts rejected; old credentials never reauthorized; known rejection/cancelled/shutdown paths nonsticky | Go unit + Python-IPC integration tests (`TestRevision*`) | fresh | pass — `go test ./...` exit 0, 18 packages (log `rework-20261010/go-test-all.txt`); Go test-case skip count not recorded (not verbose) |
| AC2 | Cert/security config: live bundle + `ssl_insecure` rejected in Go launch validation and by real mitmdump startup; safe setting and legacy accepted; lazy handshake verifies upstream | Go env tests, real mitmdump startup tests | fresh | pass — 3/3 `LiveSslInsecureStartupTest` under real mitmdump |
| AC3 | ECH opaque: extension 0xfe0d (incl. GREASE) detected from the real extension list; opaque ignore_connection, no token, zero credential visibility; malformed data denies | subprocess-isolated raw-wire tests parsed by real `mitmproxy.tls.ClientHello` + real-mitmdump boundary test | fresh | pass — 2/2 `test_ech_wire` incl. real-mitmdump origin-vs-mitm cert contrast |
| AC4 | H1 authority: exactly one strict Host equal to SNI and admission token; absolute target agreement; CONNECT/dup/missing/malformed/H2 denied before injection | unit + subprocess-isolated real `read_request_head` tests | fresh (unit/parser); deferred (Docker raw H1 inputs, image layer) | pass — 17 real-parser worker cases; image layer deferred |
| AC5 | Real Vault lifecycle: rotation on keepalive, stale expectedRevision conflict, removal fence, remove/re-add | real mitmdump runtime tests; Docker image run | fresh (loopback); deferred (image run, user decision) | pass (loopback) — 344 Python tests incl. 6 runtime cases, 0 skips; Vault HTTP API stale/remove-readd in real Docker remains deferred |
| AC6 | Idle/multipage drain: after_serial paging, failed close retried, no starvation, no deadline extension, full cleanup accounting | unit tests + real mitmdump idle-close loopback test + runner unit tests; image idle-drain case | fresh (unit+loopback); deferred (image layer) | pass — idle transport closes only at deadline; runner mocked-cleanup units pass |
| AC7 | Legacy unchanged: installation-only receiver, pull-based vault path and non-live envs keep existing behavior | existing tests kept green | fresh | pass — included in the 344-test discovery |

The Docker image layer (real transparent port-443 interception, container
lifecycle) is **deferred by user decision**: image acceptance is deferred to
GitHub CI or a later session. It has not run, no CI has passed, and this
deferral is not an acceptance downgrade — the code is prepared so the image
suite can run unchanged once authorized. The docs-site build also remains
deferred (offline dependency store incomplete).

## Evidence layers

Unit tests do not substitute for the real-runtime run, and no environment
skip is reported as a pass. Fresh per-command logs for this session are
written under the session evidence directory (`/tmp/live-vault-evidence-final/`),
one child directory per run, and are linked from the handoff report rather
than copied into this page.

- Go unit and cross-language IPC integration: `cd components/egress && umask 022 && GOPROXY=off GOSUMDB=off go test . -run 'TestRevision(VaultMutation|WriteHandler|Recovery)'` and `go test ./pkg/mitmproxy ./pkg/revisionruntime`.
- Python addon unit tests: `python -m unittest discover -v -s tests -p "test_*.py"` under the provisioned mitmproxy 11.0.2 venv.
- Real-mitmdump runtime tests (loopback only, disposable): `test_credential_bound_runtime.py`, `test_mitmproxy_runtime.py`, ECH/H1 wire tests.
- Real Docker image end-to-end: `components/egress/scripts/test_live_vault_image.py --image <egress image> --artifacts <evidence dir>` — **deferred by user decision** (to GitHub CI or a later session); not run in this session.

## Scope exclusions

- HTTP/2 GOAWAY/stream drain, the fast-sandbox subject profile, dynamic
  policy and always-rule reload participation, and recovery across a complete
  sidecar replacement are out of scope.
- Dynamic DNS learning and Kubernetes deployments are not verified by this
  experiment's current evidence; the Docker-network runner pins origin
  addresses and uses static allow rules (the local Docker embedded DNS is a
  runner convenience, unrelated to runtime DNS or Kubernetes evidence).
- The Dockerfile build knobs `GOPROXY` and `APT_MIRROR` remain optional and
  empty by default; they are local-mirror conveniences for restricted build
  networks only.

## Evidence hygiene

Test output and this document contain no credential values: fixtures mask
injected headers, the origin records only header names, the runner sanitizes
known fixture secrets in recorded commands, and runtime failures are reported
through fixed error classes.

## Commit Identity Rewrite

The user authorized rewriting only the four live-Vault slice commits. Their
author and committer are now `hpliStartAgain` with the verified-account noreply
address `118508408+hpliStartAgain@users.noreply.github.com`. Each commit retains
its original tree, timestamps, and Claude co-author trailer. The evidence
commit's message now references the rewritten implementation commit.

| Original SHA | Rewritten SHA |
|---|---|
| `a90a41330029fb646be5c63de86b3d4d8f4ac40a` | `362e7136f14621e5015ab7447b9fb7eff19feeae` |
| `a6354dea8f8df4c523fa68b146e1b8d2e4df9341` | `6c6619d38b00b6672680ac4ec69a430c7959e1a9` |
| `205d6c8601997415a34b3159b6a7e027dffe8559` | `87a3133b94202480d737057bc3043684ab24c57a` |
| `f46ff91ef572589daeeb67b453ea4e8714a68a26` | `437ff10c43cf830bd3cf91e762f33b78cd1a45ad` |

The old baseline references identify the original validation history. The
security repair is a separate child commit, not part of this metadata-only
rewrite. A local backup ref retains the old published tip.
