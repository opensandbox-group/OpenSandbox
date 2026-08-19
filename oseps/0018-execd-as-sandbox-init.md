---
title: execd as Sandbox Init
authors:
  - "@Pangjiping"
creation-date: 2026-07-27
last-updated: 2026-08-18
status: implementing
---

# OSEP-0018: execd as Sandbox Init

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Architecture](#architecture)
  - [Privilege Layering Model](#privilege-layering-model)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. Becoming PID 1](#1-becoming-pid-1)
  - [2. Init: Reaping and Signal Forwarding](#2-init-reaping-and-signal-forwarding)
  - [3. Protecting the Control Plane From the Workload](#3-protecting-the-control-plane-from-the-workload)
  - [4. The Pre-exec Hardening Prelude](#4-the-pre-exec-hardening-prelude)
  - [5. Landlock and eBPF](#5-landlock-and-ebpf)
  - [6. Degradation and Capability Probing](#6-degradation-and-capability-probing)
  - [Configuration](#configuration)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Open Implementation Questions](#open-implementation-questions)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Implementation Status

> Updated 2026-08-18. Status: **implementing** — the phased rollout below is
> implemented on branch `feat/execd-init-mode` (Phases 1–5 plus the server
> switch); remaining items are the trusted stop channel (§3), kernel-5.10
> empirical validation, cross-language e2e, and the default-on rollout (see
> the Remaining work table below). R-i (server-path hardening e2e, docker
> bridge) landed in PR #1554.

| Phase | Scope | Status |
|---|---|---|
| 1 | execd `--init` mode: single reaper (only wait4 caller), managedProcess abstraction replacing `Cmd.Wait` on every launch path, signal forwarding (TERM/HUP/USR1/USR2/WINCH), entrypoint-owned container lifecycle, subreaper fallback, `PR_SET_DUMPABLE`, `bootstrap.sh` `EXECD_INIT` exec branch, hardening endpoint reporting | ✅ implemented |
| 2 | Pre-exec floor (`[hardening] enabled`): `opensandbox-launcher` native helper (env strip → KEEPCAPS → bounding trim → no_new_privs → identity drop → ambient caps → seccomp last → execve), `[seccomp] deny` reuse with `execve` reserved, `keep_capabilities`; fail-open per layer | ✅ implemented |
| 3 | Landlock (`[landlock] enabled`): allowlist policy (system paths + `/proc/self` + device files + `/tmp`/`/run`/`allowed_writable` + extras), ABI probe v1–v4 with access-bit trimming; ABI < 1 → `unsupported` | ✅ implemented |
| 4 | eBPF observation (`[ebpf] enabled`, `execd-ebpf` variant): exec/connect/privilege hooks (CO-RE), sandbox cgroup filter, rotating JSONL audit file; kernel ≥5.10 with BTF | ✅ implemented |
| 5 | Pool taskTemplate: with `execd_run_as_init`, tasks are no longer backgrounded — execd becomes the task process-tree root (subreaper + exit-code propagation); `kill 1` recycle contract confirmed compatible under current SIGTERM semantics | ✅ implemented (task level) |
| — | Server switch `runtime.execd_run_as_init` injects `EXECD_INIT=1` across Docker / K8s Batch/Agent / Pool paths — resolves Open Question 1 | ✅ implemented |

**Open implementation questions — resolution:**

1. **Single enable switch** — resolved: the server sets `EXECD_INIT` from the
   single `runtime.execd_run_as_init` config (default `false`); `bootstrap.sh`
   passes `--init` iff `EXECD_INIT` is truthy, so topology and flag stay in
   lockstep by construction.
2. **External SIGTERM forwarding** — implemented: SIGTERM is forwarded to the
   entrypoint and execd exits with the workload's status. Distinguishing
   external vs in-namespace SIGTERM (the §3 trusted-stop mechanism) is still
   open: today an in-namespace `kill 1` also stops the sandbox (same as the
   pre-OSEP bootstrap behavior) — tracked as remaining work.
3. **Managed-process abstraction** — implemented (reaper delivers
   `WaitStatus`; per-call-site pipes/teardown are owned by the abstraction).
4. **Landlock device files** — resolved: `null/zero/full/random/urandom/tty`
   writable + `/dev/pts` subtree for the controlling terminal.
5. **Landlock `/proc/self` for descendants** — resolved by accepting the
   limitation: only the initial workload keeps `/proc/self` access; forked
   descendants get EACCES on their own procfs (documented in
   `docs/components/execd.md`).

**Remaining work** (status 2026-08-18, PR #1474 + #1546 + #1554):

| # | Item | Status / plan |
|---|---|---|
| R-a | Trusted out-of-band stop channel (§3, R2) | **Open — biggest remaining item, separate PR.** Any SIGTERM (incl. in-namespace `kill 1`) still stops the sandbox (same as pre-OSEP behavior); `kill -9 1` is inert. Target: authenticated channel unreachable from the workload; then point the K8s Restart recycle at it instead of `kill 1` (`restart_default.go`; current SIGTERM semantics confirmed compatible, comment added) |
| R-b | Pool pod-level PID 1 | **Declined follow-up.** Pool sandboxes run execd as task-level subreaper (Phase 5): the per-child floor holds without PID 1; the lost signal shield is no regression (pool exposure equals the pre-OSEP era; task-executor owns pod reaping). Operators wanting full PID 1 configure the Pool template command manually (`bootstrap.sh` + keepalive + `EXECD_INIT=1`); server auto-injection out of scope |
| R-c | Kernel-5.10 eBPF empirical validation | Open. The 5.10–5.15 exec-hook fallback (inline `filename[1024]`, `__data_loc` argv) is derived from the tracepoint definition, not boot-tested; worst case the hook degrades (fail-open). Needs a real 5.10 node with BTF + `CAP_BPF` |
| R-d | Cross-language SDK e2e | Python covered (docker-bridge + k8s nightly: PID 1, reaping, kill-9 inert, `/proc/1/environ` denial, capabilities endpoint). JS/Go/C#/Java/Kotlin init-mode e2e not written |
| R-e | `execd-ebpf` server-side selection | **Deferred.** The default image ships both binaries (`/execd` + `/execd-ebpf`); choosing which runs (`EXECD` env, `runtime.execd_binary`) is not wired into the server or the Docker/K8s distribution paths. Tracked in `components/execd/README.md` "Known issue / TODO" |
| R-f | Default-on rollout | `runtime.execd_run_as_init` and `[hardening] enabled` default `false` by design; flip after N releases of validation (owner decision), record in release notes |
| R-g | `OPENSANDBOX_ID` reserved-env override (Codex round 7) | **Deferred.** Docker env builder appends `OPENSANDBOX_ID` after user env (pre-existing pattern); harden reserved-key filtering first if a duplicate-key spoofing path is demonstrated |
| R-h | CI flake observation | PauseResume v1.32.2 (only) timed out at 900s on "commit/push fails with invalid registry" (Kubernetes CI); unrelated to this branch's recent commits — re-run to confirm flake |
| R-i | Server-path hardening e2e (Python) | **Implemented (docker bridge + k8s)** — `tests/python/tests/test_execd_hardening_e2e.py` + `scripts/python-execd-hardening-e2e.sh` + CI job `python-execd-hardening-e2e` (PR #1554), extended to the Kubernetes path in the execd-init k8s nightly. **Docker**: the hardened isolation TOML (`components/execd/configs/isolation.hardened.toml`) is injected into every sandbox via a config-level bind mount + `EXECD_ISOLATION_CONFIG` (`[docker] sandbox_env`); the workspace bind additionally exercises the launcher's mount expansion. **Kubernetes** (no server config change): the TOML travels in a ConfigMap (`opensandbox-e2e-execd-isolation`) mounted by the e2e `batchsandbox_template_file` (added `execd-isolation` volume + mount, `optional: true`, merged by the existing template-extras path), the test points `EXECD_ISOLATION_CONFIG` at it per request env, and the workspace PVC is mounted at `/mnt/workspace-exec` via request volumes so the Landlock bind-mount expansion is still exercised. k8s root-cause note: the e2e PVC's hostPath PV used to live under the kind node's `/tmp`, which is a **noexec tmpfs** — every PVC mount was therefore non-executable (writes/reads fine, exec EACCES regardless of Landlock, pod spec and CR were always correct); the e2e harness now places the PV on the node rootfs (`/var/opensandbox-e2e`, `scripts/common/kubernetes-e2e.sh`). The entrypoint dump goes to `/workspace` (writable in both runtimes) and is read back via the SDK files API on k8s. Covers: reduced caps/seccomp/NNP + env strip on entrypoint and `/command`, Landlock (`/tmp` writable, `/etc/passwd` read-only, workspace mount write+exec; skipped when the kernel reports `unsupported`, per §6 fail-open), capabilities endpoint layer states, and the missing-`CAP_SETPCAP` degradation (phase 2, docker only — k8s degradation still open: the k8s container ceiling caps are not tuned in the e2e) |
| R-j | eBPF JSONL audit e2e | **Open.** No container/e2e test runs the `execd-ebpf` variant with `[ebpf] enabled` and asserts exec/connect/privilege events land in the rotating audit file; only event-decoding unit tests exist — the `commit_creds` privilege hook has never been validated on a real kernel (ties into R-c/R-e) |
| R-k | Python e2e signal-forwarding breadth | **Implemented** — `test_application_signals_forwarded_to_entrypoint` now sends HUP/USR1/USR2/WINCH to PID 1 and asserts every trap marker fires in the entrypoint (SIGTERM graceful shutdown covered by `tests/init_container.sh`) |
| R-l | K8s init-mode e2e depth | **Partially addressed.** The k8s nightly runs the same Python file with two k8s-path adaptations: `test_entrypoint_exit_code_propagates` skips on k8s (BatchSandbox stays Pending after pod completion and does not surface the container exit code — verified empirically on the nightly), and the `kill 1` pin asserts execd becomes unreachable instead of a lifecycle state transition. Still open: no Pool + `EXECD_INIT` subreaper-report case (R-b); no K8s Restart recycle (`kill 1`) against an init-mode pod — the `restart_default.go` "contract compatible" comment is unverified e2e |
| R-m | Default-off assertion + sustained fork-heavy | **Open (low).** No explicit e2e pin that with init/hardening off the capabilities endpoint reports `init_mode: none` and layers `disabled`; the fork-heavy e2e loop is 20×5 short-lived children — a long-running mix of `/command` churn + background sleepers would closer match OSEP §Test-Plan |
| R-n | PTY path under hardening — zero coverage | **Implemented** — Go integration test `TestHardeningPTYSessions` (`hardening_linux_test.go`, runs in the execd `test` CI job): StartPTY + StartPipe both launch through the launcher with the reaper active and assert the session shell reports Seccomp=2 / NoNewPrivs=1 / CapEff=0 (root) / `EXECD_ACCESS_TOKEN` stripped. Container-level `/pty` WS case dropped: the alpine execd image has no WS client, and the pty fd / `setsid` / `Setctty` survival across the launcher's `execve` is covered by the integration test |
| R-o | Isolated session (bwrap) + init-mode reaper combination | **Implemented** — (1) Go integration test `TestIsolatedSessionWithInitReaper` (`isolated_session_initmode_linux_test.go`, `linux && bwrap`, run as root in the `bwrap-smoke` CI job): full bwrap lifecycle (create/run/exit-code/delete) under reaper dispatch, plus a delete racing a running workload to exercise the pre-reap barrier's PGID-reuse serialization with the reaper's WNOWAIT-observe → consume path. (2) Python e2e `TestIsolatedSessionHardeningE2E` (docker bridge, runs in the hardening e2e job's phase 1): bwrap sessions under init mode + the floor — capabilities available, session workload carries bwrap's seccomp/NNP floor + credential env strip, PID-namespace isolation, state persistence, delete-while-busy teardown, hardening report intact around sessions |
| R-p | `/code` (Jupyter kernels) under init/hardening e2e | **Declined follow-up.** Not validated at e2e level; kernels inherit the reduced Jupyter entrypoint by construction, and `test_execd_init_e2e.py` covers Jupyter startup under PID 1 via the ready check. Revisit if the code-interpreter entrypoint changes |
| R-q | Custom `[seccomp] deny` + `keep_capabilities` e2e | **Open (low).** Only Go unit tests cover the reserved-`execve` rejection (`TestHardeningRejectsReservedExecve`) and the ambient-raise path (`TestHardeningKeepCapabilities`). No e2e runs a hardened sandbox with a custom deny list + `keep_capabilities`: plan a TOML variant (e.g. deny `chmod`, `keep_capabilities=["CAP_NET_RAW"]`) in the hardening e2e asserting the denied syscall fails in `/command` and the workload shows CapEff=0x2000 |
| R-r | e2e consumes the SDK `hardening` model instead of a raw HTTP probe | **Implemented** — `_hardening_report` now reads `sandbox.isolation.capabilities().hardening` (`HardeningStatus` model) instead of a `/command` urllib JSON probe, pinning the spec → SDK → implementation alignment of the hardening object |
| R-s | `EXECD_INIT` ↔ TOML drift pin (init off, hardening on) | **Open (low).** The "hardening enabled but execd is not the init → layers degraded" state is only pinned in Go (`TestHardeningReportDegradesWithoutInitMode`); no Python e2e asserts the endpoint reports `init_mode: none` + degraded layers with `[hardening] enabled` but `execd_run_as_init = false`. Complements R-m's default-off pin |
| R-t | Reaper sweep backstop (lost/coalesced SIGCHLD) | **Implemented** — `TestReaperSweepBackstop` (`initmode_linux_test.go`): the reaper's `signal.Notify` subscription stays registered (blocking the Go runtime's auto-reap) while the run loop is severed from it, so only the sweep ticker can reap an exiting child; asserts the child is drained within the ticker budget |
| R-u | Runtime-initiated container stop (external SIGTERM) at SDK/e2e level | **Open (low).** Only `components/execd/tests/init_container.sh` (docker stop) covers the graceful-shutdown path; no SDK-level e2e asserts the entrypoint receives SIGTERM and the sandbox exits with its status when the runtime stops the container. Adjacent to R-k's signal-forwarding breadth |

Closed this round (2026-08-12): CI green for all execd-init jobs; `/proc/1/environ`
e2e assertion (`test_workload_cannot_read_execd_environ`); hardening report
degrades honestly when hardening is on without init topology; launcher aborts on
failed identity drop; bundled-image Jupyter log relocated under `/tmp`;
generated eBPF bindings gated behind `ebpf &&` tags; `runtime.execd_run_as_init`
documented in `server/configuration.md`.


## Summary

This proposal makes **execd** the sandbox init (PID 1): it `fork`/`exec`s the user
entrypoint, reaps zombies, and forwards signals. execd also becomes the
**authorizer** — every process that runs user code (the entrypoint and anything
execd spawns for `/command`, `/code`, PTY, or isolated sessions) is launched through
one pre-exec hardening prelude that drops it below execd's (and the container's)
privileges: reduced capabilities, `no_new_privs`, a seccomp filter, and optional
Landlock. Optional eBPF observation audits exec/connect/privilege events to a local
file.

The hardening primitives already exist for *isolated sessions* (OSEP-0013): execd
runs bubblewrap with a seccomp denylist and a `setpriv` identity drop. This OSEP
generalizes that "execd privileged, children reduced" pattern to the whole sandbox
and adds init duties to execd, with no separate supervisor process.

## Motivation

OpenSandbox runs untrusted, AI-generated code. Today the container entrypoint is
`bootstrap.sh` (`components/execd/Dockerfile`), which backgrounds execd (`$EXECD &`)
and the user command (`"$@" &`) as **siblings** and `wait`s on the user PID. So PID 1
is a shell, execd is not the parent of the workload, and the strong isolation lives
only inside per-execution bubblewrap sandboxes. This leaves three gaps:

- **Zombie accumulation** — orphaned children reparent to a shell that reaps nothing.
- **Privilege parity** — the workload inherits the container's full caps/seccomp/FS;
  nothing makes it *less* privileged than the container.
- **No audit signal** — no lightweight view of what the workload did at the syscall
  level.

execd is already the always-present control plane in every sandbox, so making it the
init consolidates the trusted computing base into one process and lets it act as the
privilege authorizer for all user code.

### Goals

1. **execd is the sandbox init**: parent of the entrypoint, reaps zombies, forwards
   signals.
2. **Layered privileges**: execd keeps the container ceiling; every user-code process
   runs with a strict subset (reduced caps, `no_new_privs`, seccomp, optional
   Landlock), applied through **one** launch path.
3. **Workload cannot kill the control plane**, enforced by the PID 1 kernel signal
   shield; trusted stop/recycle uses an authenticated out-of-band channel, not an
   in-namespace signal (§3).
4. **Reuse** the existing `isolation` seccomp generator and `setpriv`/namespace code.
5. **Optional eBPF observation**, opt-in and capability-probed.
6. **Fail-open**: any missing kernel/capability support is logged, reported, and
   skipped — never fatal.

### Non-Goals

1. **Replacing container-runtime isolation** — this is in-container defense-in-depth,
   not a substitute for gVisor/Kata (OSEP-0004) or Kubernetes controls.
2. **Granting execd more than the container allows** — Linux privileges only reduce;
   "more privilege for execd" is an operator raising the container ceiling (see
   [Privilege Layering Model](#privilege-layering-model)).
3. **Windows** — hardening is Linux-only.
4. **A general service manager** — one user entrypoint per sandbox.
5. **eBPF enforcement/blocking** — initial scope is observation only.
6. **Per-request tuning** — hardening is an operator/image decision, not a
   `CreateSandboxRequest` field.

## Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| R1 | execd is the sandbox init: reaps zombies, forwards signals | Must |
| R2 | Workload children cannot terminate/restart execd; trusted stop/recycle is via an authenticated out-of-band channel, not an in-namespace signal (reconcile with the K8s `kill 1` recycle path — §3) | Must |
| R3 | execd retains the container ceiling; every user-code process is dropped to a strict subset, via one shared pre-exec prelude | Must |
| R4 | Default seccomp reuses `isolation`'s generator; operators may override seccomp/Landlock via the isolation TOML | Must |
| R5 | Landlock applied when the kernel supports it; absent → non-fatal | Should |
| R6 | eBPF observation is opt-in, capability-probed, writes structured events to a file | Could |
| R7 | All features fail open (log + skip, never abort startup); actual state reported on the capabilities endpoint | Must |
| R8 | Default behavior unchanged unless explicitly enabled | Must |
| R9 | execd protects its own liveness as PID 1 (panic isolation + deterministic exit) | Must |

## Proposal

### Architecture

```
Container ENTRYPOINT
└── execd (PID 1)                       ← control plane AND sandbox init
    │     reaps zombies · forwards signals · signal shield · authorizer
    │
    ├── user entrypoint          ─┐
    ├── /command, /code           ├─ all via ONE pre-exec launcher (§4 for the
    ├── PTY sessions              │   exact, order-sensitive sequence): strip creds
    └── isolated sessions (bwrap) ─┘   → trim caps → no_new_privs → setpriv → ambient
                                       → Landlock → seccomp (last) → exec
                                       (⇒ privilege < execd < container ceiling)
    │
    └── (optional) eBPF observer (sandbox cgroup) → local rotating JSONL audit file
```

`bootstrap.sh` shrinks to environment/CA-trust setup that `exec`s into execd, so
execd inherits PID 1 (Design §1). The user command becomes a first-class execd child
instead of a backgrounded sibling.

### Privilege Layering Model

Linux privileges are **reducible only** — a process can drop privilege but never gain
more than it was granted. Hence three layers:

```
CEILING   Container caps / seccomp / LSM (Docker cap_drop, K8s securityContext)
(operator)         │  set by operator; nothing inside can exceed it
                   ▼
execd     Runs at the full ceiling, does NOT self-drop. Needs CAP_SETPCAP to trim
(authorizer)       │  child bounding sets, /proc for the gate, setpriv, namespaces.
                   ▼  fork → in CHILD before exec, order-sensitive (§4 is authoritative):
                   ▼  strip creds → trim caps → no_new_privs → setpriv → ambient → Landlock → seccomp(last)
user code Reduced caps (default none), no_new_privs, seccomp, Landlock FS scope,
(floor)            dropped uid — cannot regain anything (no_new_privs + trimmed bounding set)
```

- **"More privilege for execd"** = the operator raises the ceiling (fewer
  `drop_capabilities` / K8s `capabilities.add`) and execd does not self-drop.
- **"Less privilege for the workload"** = execd's per-launch prelude subtracts from
  the child only, **after `fork`, before `exec`** — execd's own credentials are never
  touched (mirrors the isolated-session path).

This must cover **every** user-code path (entrypoint, `/command`, `/code`, PTY,
isolated sessions); any launch that skips the prelude would inherit execd's ceiling.
The prelude is therefore the single mandatory launch primitive (Design §4).

### Risks and Mitigations

| Risk | Mitigation |
|------|------------|
| execd crash (PID 1) kills the sandbox; no inner restarter | Keep init core small; recover HTTP/Jupyter panics; deterministic exit lets the outer runtime restart the container (crash backstop only — see §3) |
| A user-code path skips the prelude → inherits the ceiling | Prelude is the single mandatory launch primitive; test every path (§4, Test Plan) |
| Reaper races with execd's own `Wait()` | Single reaper is the only `wait4` caller; execd never calls `Cmd.Wait()`; the reaper dispatches each owned child's status to its registered waiter (§2) |
| Over-restrictive seccomp/Landlock breaks images | Conservative defaults (reuse E2E-tested denylist); everything overridable; fail-open |
| Operator ceiling too low (e.g. no `CAP_SETPCAP`) | Probe + report on capabilities endpoint + log; degrade, never block (§6) |
| eBPF needs CGO/caps/BTF | Opt-in, off by default, separate build variant; default image unchanged |
| In-namespace `kill 1` can't be distinguished from a hostile workload signal | Stop/recycle is not signal-driven; use an authenticated out-of-band channel. **Open item:** reconcile with the K8s `kill 1` recycle contract before enabling init mode on that path (§3) |

## Design Details

> Code snippets are illustrative.

### 1. Becoming PID 1

Today PID 1 is `bootstrap.sh`, which stages/launches execd across three paths:

- **Docker** (`docker/container_ops.py`): `entrypoint=[bootstrap.sh]`, `command=[user cmd]`.
- **K8s Batch/Agent** (`k8s/provider_common.py`): `command=[bootstrap.sh, entrypoint…]`;
  execd is copied into the shared `/opt/opensandbox` volume by an `execd-installer`
  init container.
- **K8s Pool taskTemplate** (`k8s/batchsandbox_provider.py`): `sh -c "bootstrap.sh <entrypoint> &"`.

`bootstrap.sh` also does real setup that must be preserved: egress MITM CA install
(system/NSS/JDK trust stores), `REQUESTS_CA_BUNDLE`/`SSL_CERT_FILE`/`NODE_EXTRA_CA_CERTS`,
`EXECD_BOOTSTRAP_PRE_SCRIPT`, `EXECD_ENVS`, **and command normalization** — it accepts
`BOOTSTRAP_CMD`, the `bootstrap.sh -c "…"` form, and `BOOTSTRAP_SHELL` selection, not
just a plain argv (`components/execd/bootstrap.sh:298-330`).

**The process topology is chosen by `bootstrap.sh`, before execd runs, via a
bootstrap-visible env var.** execd cannot decide its own PID: whether it `exec`s
(becoming PID 1) or backgrounds is fixed by the shell *before* execd can parse its
TOML, so a TOML-only switch is insufficient. init mode is therefore gated by an env
var the shell reads — `EXECD_INIT` (set by the server alongside the existing execd
env, in lockstep with the TOML `[hardening] enabled` that turns on the floor):

```sh
# tail of bootstrap.sh, after the CA-trust/env setup:
if is_truthy "${EXECD_INIT:-}"; then
    exec "$EXECD" --init -- <normalized user cmd>   # execd becomes PID 1
else
    "$EXECD" & ; <normalized user cmd> & ; wait "$!"  # today's behavior, unchanged
fi
```

So **default off (`EXECD_INIT` unset) reproduces exactly today's background-and-wait
tail** — the compatibility guarantee is implementable because the switch lives where
the topology decision is actually made. **Command normalization stays in
`bootstrap.sh`**: the shell resolves `BOOTSTRAP_CMD` / `-c` / `BOOTSTRAP_SHELL` into
the concrete argv and passes *that* to execd (both branches), so existing forms keep
working and execd need not re-implement the shell logic. Per-path wiring (server sets
`EXECD_INIT` + the flag; no API/CRD change):

| Path | With this OSEP | execd becomes PID 1? |
|---|---|---|
| Docker | `bootstrap.sh` ends in `exec execd --init -- <user cmd>` | **Yes** — it is the container entrypoint |
| K8s Batch/Agent | same; `execd-installer` stages execd + updated `bootstrap.sh` | **Yes** — `command` is the container entrypoint |
| K8s Pool | see below | **No, not as-is** |

**Docker and K8s Batch/Agent reach PID 1 directly**, because `bootstrap.sh` *is* the
container's entrypoint, so `exec` (no `&`) makes execd inherit PID 1.

**K8s Pool is different and needs a structural change (open item).** The Pool
taskTemplate command is not the container entrypoint — it is submitted to an
already-running **task-executor**, which starts it with `exec.Command`
(`kubernetes/internal/task-executor/runtime/process.go`). `exec` there only replaces
the task's child shell, **not** the container's existing PID 1, so a Pool sandbox
would fall into subreaper mode (no signal shield, workload-subtree reaping only).
Making execd the real PID 1 for pooled sandboxes requires changing the **pool Pod
entrypoint** (make execd/`bootstrap.sh` the Pod's PID 1 and have the task-executor
hand the user command to that execd), not just editing the task command. Until that
is done, **pooled sandboxes run execd in subreaper mode** — this is called out
honestly rather than claimed as full PID 1. Tracked as a required follow-up for the
Pool path.

All of this is gated by `EXECD_INIT` (topology) in lockstep with `[hardening]
enabled` / `--init` (runtime); unset → current background-and-wait behavior, so
existing sandboxes are unchanged. On the direct paths, failing to `exec` (a stray
`&`) is a misconfiguration that also degrades to a
subreaper with no signal shield (§6).

### 2. Init: Reaping and Signal Forwarding

```go
if os.Getpid() != 1 { // subreaper mode: Pool path (pre-restructure) or misconfig (§1,§6)
    _ = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}
go reapLoop()                                 // the ONLY caller of wait4 in execd
entry := startTracked(argv, harden, roleEntrypoint) // container-lifecycle owner
forwardSignals(entry.Pgid)                    // HUP/USR1/USR2/WINCH → workload
status := <-entry.done                        // block on the entrypoint's status
shutdownOtherChildren()                       // SIGTERM→grace→SIGKILL the rest
os.Exit(exitCodeFrom(status))                 // propagate to Docker/kubelet
```

**Single reaper, status dispatch, no registration race.** `wait4(-1, …)` atomically
*selects and consumes* a child's status, so a loop cannot inspect the PID and "skip"
an owned one; it would steal the status a concurrent `os/exec.Cmd.Wait()` needs,
leaving `ECHILD` and no exit code. So **execd never calls `Cmd.Wait()` for its own
children** — the single `reapLoop` is the *only* `wait4` caller.

Because a PID only exists after start succeeds, "register before launch" is not
literally possible and a short-lived child could exit before registration. To close
that race, the reaper keeps a **pending-status map**: a status for an as-yet
unregistered PID is buffered, not discarded, and delivered when the owner registers
(and a registry lock spans start+registration):

```go
func reapLoop() {
    for range sigchld {
        for {
            var ws unix.WaitStatus
            pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
            if pid <= 0 || err == unix.ECHILD { break }
            if ch := owners.take(pid); ch != nil {
                ch <- ws                 // registered owner: deliver status
            } else if owners.isTracked(pid) {
                owners.stashPending(pid, ws) // started but not yet registered
            } else {
                emitReapEvent(pid, ws)   // reparented orphan: reap + audit
            }
        }
    }
}
// register() first drains any pending status for its pid before waiting.
```

**Banning `Cmd.Wait` means reproducing its cleanup.** `os/exec.Cmd.Wait` does more
than collect a status: it joins the stdout/stderr copy goroutines, closes the pipe
FDs, populates `ProcessState`, and releases `Cmd` resources. The `/command`, bash,
PTY, and isolated-session paths rely on that. So execd introduces a small
**managed-process abstraction**: it still builds the child with `os/exec` (pipes,
env, `Setpgid`) and calls `Start`, but instead of `Wait` it (1) waits on the
reaper-delivered `WaitStatus`, then (2) runs the same teardown `Wait` would —
drain/close pipes, join copy goroutines, synthesize `ProcessState` from the status.
The reaper owns *only* the `wait4` syscall; per-child I/O and resource lifecycle stay
with this abstraction, so nothing leaks and output handling stays complete.

**Entrypoint owns the container lifecycle (#exit propagation).** Unlike a
`/command`/PTY child, the **user entrypoint** determines container liveness: when it
exits, execd stops the other children (SIGTERM→grace→SIGKILL), then exits with the
entrypoint's status so Docker/kubelet observe it — matching today's
`bootstrap.sh` `wait "$CMD_PID"; exit $?`. A finishing entrypoint must **not** leave
the execd HTTP daemon (and thus the container) running. Application signals
(HUP/USR1/USR2/WINCH) are forwarded to the workload, not acted on by execd.

### 3. Protecting the Control Plane From the Workload

**Property: untrusted workload code cannot terminate or restart execd.** As PID 1,
the kernel delivers to execd only the signals it has installed a handler for;
everything else from in-namespace processes (incl. `SIGKILL`/`SIGTERM`) is discarded
(`pid_namespaces(7)`). This is a property of being PID 1 (§1), not code execd writes,
and there is no equivalent in the non-PID-1 fallback (§6).

**Signals cannot distinguish platform recycle from a hostile workload — so stop is
not signal-driven.** The Kubernetes Restart recycle strategy runs
`DefaultRestartCommand = ["kill", "1"]` via **Pod exec**
(`kubernetes/internal/controller/recycle/restart/restart_default.go`). That `kill`
executes *inside* the container's PID namespace, so its `SIGTERM` carries a **nonzero
`si_pid`** — indistinguishable from a workload child running `kill 1`. There is no
reliable in-signal way to tell "trusted recycle" from "hostile workload." Two
consequences:

- **execd installs no workload-reachable stop handler.** Because it cannot safely act
  on an in-namespace `SIGTERM`, execd does not treat `kill 1` as a stop request; the
  signal shield then makes a workload's `kill 1`/`kill -9 1` inert.
- **Trusted stop needs an out-of-band, authenticated channel** — not a signal. The
  concrete mechanism is deferred to implementation, but the requirement is explicit:
  a stop/recycle request must be distinguishable from any in-namespace signal.
  Candidate channels: (a) an authenticated request on execd's control API; (b) a
  truly out-of-namespace signal (e.g. the runtime's container-stop `SIGTERM` to PID 1
  during Pod deletion, which *is* external), which execd may honor; (c) changing the
  K8s Restart recycle strategy to target execd's stop endpoint instead of `kill 1`.

> **The trusted-stop credential must not be recoverable by the workload.** Candidate
> (a) cannot simply reuse the existing `EXECD_ACCESS_TOKEN`: that token is passed via
> environment (`components/execd/pkg/flag/parser.go`), and when the workload shares
> execd's UID (non-root image, or a degraded cap-drop) it could read
> `/proc/1/environ` to steal it — especially since the Landlock defaults (§5) allow
> reading `/proc`. That would let the workload call the stop endpoint and defeat the
> whole property. Therefore trusted stop must use a credential/channel the workload
> **cannot** reach: e.g. a socket/fd held only by execd and reachable only from
> outside the sandbox PID namespace (candidate b/c), or a secret never exposed via
> env or `/proc`. Correspondingly, the Landlock defaults must **not grant a procfs
> subtree that includes `/proc/1`** — they grant `/proc/self` (and specific read-only
> procfs files), never all of `/proc` (Landlock can only allow, not allow-then-
> exclude; §5).
>
> **More directly, the launcher must strip execd's credential env from the workload's
> own environment.** Today the non-isolated paths pass `os.Environ()` to commands/PTYs
> (`command.go`, `pty_session.go`) and the entrypoint inherits the container env, so a
> workload would find `EXECD_ACCESS_TOKEN` in *its own* environment without touching
> `/proc` at all. The launcher therefore unsets execd's config/credential vars before
> `execve`, reusing the existing `execdConfigEnvBlacklist` that isolated sessions
> already strip via bwrap `--unsetenv` (`EXECD_ACCESS_TOKEN`, `JUPYTER_TOKEN`,
> `EXECD_ISOLATION_CONFIG`, `EXECD_ENVS`, …). Env-strip + no-`/proc/1` together close
> the token-theft path; both are hard requirements.
>
> **A same-UID workload must also be unable to `ptrace` execd.** If the workload runs
> at execd's UID and the seccomp layer fails open (or a custom denylist omits
> `ptrace`/`process_vm_readv`/`process_vm_writev`), a kernel without a restrictive
> Yama `ptrace_scope` would let the workload attach to or rewrite the memory of the
> (dumpable) execd process — bypassing the signal shield and the credential
> protections above. execd therefore marks itself **non-dumpable**
> (`prctl(PR_SET_DUMPABLE, 0)`), which denies same-UID `ptrace`/`process_vm_*` against
> it regardless of Yama; the default seccomp denylist already blocks these syscalls
> for the workload, and this is the belt-and-suspenders for the fail-open/custom-deny
> case. (Running execd under a distinct UID from the workload is an even stronger
> option where the image permits it.)

> **Open item / compatibility risk.** Option (c) means the existing `kill 1` recycle
> contract does not work as-is against an init-mode execd, since execd deliberately
> ignores in-namespace `kill 1`. This must be reconciled with the controller
> (`restart_default.go`) — either the recycle command changes, or init mode ships
> with a documented alternative recycle path. Tracked as a required follow-up before
> init mode is enabled on the K8s Restart path.

**Self-resilience (crash, not attack).** Init/reaper/signal goroutines are isolated
from HTTP handlers (Gin recovers handler panics). On an unrecoverable fault execd
stops the child (SIGTERM→grace→SIGKILL) and exits deterministically; reviving a dead
PID 1 is only possible from the outer runtime (kubelet `restartPolicy` / Docker
`--restart`) — a crash backstop only, not the normal path.

### 4. The Pre-exec Hardening Prelude

The prelude must run in the child **between fork and exec**, applying steps that must
not touch execd itself. **Go's `os/exec` offers no hook to run arbitrary Go code in
the child at that point** (`creack/pty` also just wraps `Cmd.Start`), so the prelude
cannot be a Go callback. It is instead a **tiny native launcher** that execd `exec`s
as the child's `argv[0]`; the launcher applies the steps and then `execve`s the real
user command. This is the same shape execd already uses for isolated sessions — the
`opensandbox-session-gate` native helper (`components/execd/native/session-gate.c`)
that runs as the last barrier before a bwrap workload — so this OSEP **extends that
helper** rather than inventing a new mechanism.

Launch shape:

```
execd → exec("opensandbox-launcher", <policy fds/args>, "--", <user argv>)
            │  in the launcher process, before execve(user argv):
            │    0. unset execd's credential env (execdConfigEnvBlacklist:
            │       EXECD_ACCESS_TOKEN, JUPYTER_TOKEN, …) from the child environment
            │    1. prctl(PR_SET_KEEPCAPS, 1)            # keep caps across the uid change
            │    2. PR_CAPBSET_DROP for every cap not in KeepCaps   # needs CAP_SETPCAP
            │    3. prctl(PR_SET_NO_NEW_PRIVS, 1)
            │    4. setgroups + setgid + setuid  (still holds CAP_SETUID/SETGID)
            │    5. set exactly KeepCaps in permitted/effective/inheritable (capset);
            │       for each KeepCap: PR_CAP_AMBIENT_RAISE  # survive execve to the workload
            │    6. Landlock ruleset (§5)
            │    7. seccomp filter (BPF) — LAST, so it never blocks the launcher's own
            │       setuid/capset/prctl/landlock setup calls above
            └─► execve(user argv)     # with the credential-stripped environment
```

**Ordering.** Several Linux facts pin the sequence:

- Trimming the **bounding set** (`PR_CAPBSET_DROP`) needs `CAP_SETPCAP`, so it happens
  **before** identity change (step 2, while still privileged).
- Changing UID from 0 to nonzero **clears the permitted/effective cap sets** unless
  `PR_SET_KEEPCAPS` is set first — otherwise `CAP_SETUID`/`CAP_SETGID` vanish and the
  UID change fails with `EPERM`. Hence step 1, and the identity-changing caps survive
  to step 4.
- **`PR_SET_KEEPCAPS` preserves caps across the UID change, not across `execve`.** A
  nonempty `KeepCaps` must additionally be raised in the **ambient** set
  (`PR_CAP_AMBIENT_RAISE`, step 5), else the workload `execve`s with *zero* caps.
  Ambient caps require the cap in permitted+inheritable and `no_new_privs` (step 3).
- **seccomp is installed LAST (step 7).** A reused `[seccomp] deny` override may list
  `setuid`/`setgid`/`setgroups`/`capset`/`prctl` (to deny them *to the workload*);
  installing the filter earlier would block the launcher's own steps 4–5 and fail the
  launch (or, under fail-open, run with execd's identity/caps). Installing it after
  all setup — but before `execve` — restricts only the workload while letting the
  launcher complete. Landlock (step 6) likewise precedes seccomp so its own syscalls
  aren't filtered.
- **The launcher's final exec syscall must not be in the deny list.** The launcher's
  final act is the `execve` into the workload (the existing native gate ends with
  `execvp`, i.e. `execve`), *after* the filter is installed and with no way to remove
  it — so a deny override containing that syscall would deny the exact transition and
  fail every hardened launch. execd therefore **validates the reused `[seccomp] deny`
  at load time and rejects the launcher's exec syscall (`execve`)** with a clear
  error. Only that one is reserved; an `execveat`-only deny (which the launcher does
  not use) stays valid, so no unnecessary compatibility/security regression. (The
  built-in default denylist does not include it.)

`no_new_privs` (step 3) prevents regaining privilege across `execve` regardless of
ordering; seccomp/Landlock precede the identity drop so they cover the workload.
After the launcher `execve`s, the child holds exactly `KeepCaps` (default: none, via
the empty ambient set) and cannot regain anything.

**Seccomp** reuses `isolation/seccomp_gen.go` verbatim — its BPF is handed to the
launcher (via fd/arg) and installed with `PR_SET_SECCOMP` instead of
`bwrap --seccomp`; filters inherit only to descendants, so execd is unaffected.
Custom denylist reuses the existing `[seccomp] deny=[…]` in the isolation TOML (no new
schema).

**Target identity (which UID/GID the drop targets).** The floor does not invent a
UID. The launcher drops to the **image/container's own runtime identity** — the user
the image declares (its `USER`, i.e. the identity a non-init-mode container would run
as), resolved once at startup and supplied to every launch path (entrypoint,
`/command`, `/code`, PTY). This keeps file ownership consistent with the image and
matches today's non-hardened behavior (where the container simply runs as that user);
hardening only *adds* caps/seccomp/Landlock on top, it does not relocate the workload
to a foreign account. If the image runs as root and the operator wants a nonroot
floor, that is the operator's ceiling/image decision, not a value this floor fabricates.
(Isolated sessions keep their existing per-request UID/GID, which is separate.)

Because the workload may thus share execd's UID (a root image, or degraded cap-drop),
execd must not expose any workload-reachable secret that grants control over it — see
the trusted-stop credential rule and the `/proc/1` Landlock denial in §3/§5.

Coverage: entrypoint, `/command`, `/code`, and PTY all launch via this launcher;
isolated sessions already reduce via bwrap + session-gate and compose under this
floor. A test asserts every path ends up with reduced caps + active seccomp.

### 5. Landlock and eBPF

**Landlock** (Linux ≥ 5.13; a Linux-only, build-tagged package) is an unprivileged
LSM: a process restricts its own and its descendants' filesystem access, irrevocably
and inherited across `fork`/`exec` — ideal for the child prelude. `applyLandlock`:

1. Probe ABI: `landlock_create_ruleset(NULL, 0, LANDLOCK_CREATE_RULESET_VERSION)`;
   `<1`/`ENOSYS` → skip (fail-open). ABI: v1=5.13 FS r/w/x, v2=5.19 `REFER`,
   v3=6.2 `TRUNCATE`, v4=6.7 network.
2. Trim handled access bits to the detected ABI (don't declare newer bits on old
   kernels).
3. Create ruleset; allowlist paths via `LANDLOCK_RULE_PATH_BENEATH` (`read_exec` →
   read+exec, `read_write` → +write); unlisted handled accesses are denied. **Rules
   only *grant* access beneath a path — Landlock cannot layer a more-specific deny to
   carve an exception out of a granted subtree.** So confinement is expressed purely
   by *which* paths are allowed, never by allow-then-exclude.
4. `landlock_restrict_self` (requires `no_new_privs`, already set).

The Landlock syscalls run **inside the native launcher** (§4), like seccomp and the
cap drop — running them in the execd Go process would restrict PID 1 itself.
(Whether the launcher is the extended C `session-gate` or a dedicated Go launcher
binary is an implementation choice deferred to that stage; either way the syscalls
execute in the child, not in execd.)

Default policy (must cover what real shells/Python/Java/native programs need, or they
break under default-deny):

- **read+exec**: system paths (`/usr`,`/bin`,`/lib`,`/lib64`,`/etc`) and read of
  `/run`. For procfs, because Landlock cannot allow `/proc` and then exclude
  `/proc/1`, the default grants **`/proc/self`** (and, if needed,
  `/proc/sys`/`/proc/cpuinfo`-class read-only paths) rather than all of `/proc` — so
  other PIDs' `environ`, including execd's `/proc/1/environ`, are never in the granted
  set. This is what actually closes the §3 same-UID token-theft path (an allow-then-
  deny on `/proc` would not).
- **read+write**: the writable device files it needs (`/dev/null`,`/dev/zero`, the
  controlling tty), `/tmp`, writable portions of `/run`, the workspace, and the
  existing `AllowedWritable` (`/workspace`,`/mnt`,`/media`,`/data`), plus
  `extra_writable`.

Allowlisted paths must exist (`O_PATH`). A read-only override (`extra_readable`) is
provided alongside `extra_writable`. The precise default set is refined during
implementation against real images, but it must cover `/proc/self`, the needed
`/dev` files, `/tmp`, and `/run` so common workloads run out of the box **without ever
granting a subtree (`/proc`, all of `/dev`) that would expose another process's
credentials**.

**eBPF observation** — opt-in, off by default, **observation only** (blocking stays
with seccomp and the egress sidecar):

```
[kernel] exec / connect / commit_creds hook  (tracepoint/LSM, CO-RE)
   └─ cgroup-id filter (sandbox cgroup only) → BPF ringbuf
        └─[execd] ringbuf.Reader goroutine → structured event (+sandbox_id)
             └─ append JSONL → rotating audit file (lumberjack; not OTel)
```

Hooks: `sched_process_exec` (pid, filename, argv), `security_socket_connect`/
`tcp_connect` (pid, dst IP:port), `commit_creds`/setuid (uid/gid, cap delta). Because
execd is PID 1 and owns the process tree, the cgroup filter reliably scopes to *this
sandbox only*. Events go to a local rotating JSONL file (reusing the `lumberjack`
setup `cmd/supervisor` already uses); shipping them off-node is an operator concern.

**Log format.** One JSON object per line (JSONL), following the same shared-envelope
convention as the supervisor event log (`internal/supervisor/events.go`): a stable
common envelope plus event-specific fields, `omitempty` so kinds share one shape.

Common envelope (every record):

| Field | Type | Meaning |
|---|---|---|
| `ts` | RFC3339 string | event timestamp |
| `event` | string | `exec` \| `connect` \| `privilege` (stable; filter on this) |
| `sandbox_id` | string | owning sandbox |
| `pid` | int | acting process pid (in the sandbox PID namespace) |
| `comm` | string | process short name |

Event-specific fields:

| `event` | Fields |
|---|---|
| `exec` | `filename` (string), `argv` (string[]), `ppid` (int) |
| `connect` | `dst_ip` (string), `dst_port` (int), `proto` (`tcp`\|`udp`) |
| `privilege` | `old_uid`/`old_gid`, `new_uid`/`new_gid` (int), `cap_added` (string[], e.g. `["CAP_NET_ADMIN"]`) |

Example lines:

```jsonl
{"ts":"2026-07-27T10:30:00Z","event":"exec","sandbox_id":"sbx-abc","pid":42,"comm":"python3","filename":"/usr/bin/python3","argv":["python3","-c","..."],"ppid":1}
{"ts":"2026-07-27T10:30:01Z","event":"connect","sandbox_id":"sbx-abc","pid":42,"comm":"python3","dst_ip":"93.184.216.34","dst_port":443,"proto":"tcp"}
{"ts":"2026-07-27T10:30:02Z","event":"privilege","sandbox_id":"sbx-abc","pid":57,"comm":"sudo","old_uid":1000,"new_uid":0,"cap_added":["CAP_SETUID"]}
```

Field names reuse existing conventions where they exist (`ts`, `event`, `sandbox_id`
matches the metrics/telemetry `sandbox_id` attribute). `event` values are a stable
contract; new observation kinds add new values, not new required envelope fields.

Constraints (why it's a separate build): needs `CAP_BPF`+`CAP_PERFMON` (held by
execd, dropped for children), a BTF kernel (≈≥5.8), and `cilium/ebpf` — which
conflicts with the default `CGO_ENABLED=0` static image, so it ships as a separate
`execd-ebpf` variant; under gVisor/Kata the host kernel is not attachable → skipped.

### 6. Degradation and Capability Probing

**Every capability is best-effort and fail-open: a missing prerequisite is logged
*with the concrete reason*, reported on the capabilities endpoint (state + reason
message, below), and skipped — never aborting startup, never silently pretending to
be active.** Layers are independent; losing one never cascades.

| Feature | Prerequisite | Degradation when unavailable |
|---|---|---|
| Cap drop | `CAP_SETPCAP` in execd | Skip cap trimming; workload keeps ceiling caps but still gets seccomp/Landlock/uid drop |
| Seccomp on child | `PR_SET_SECCOMP` allowed | Skip the seccomp floor (isolated-session seccomp unaffected) |
| Landlock | kernel ≥ 5.13 | Skip FS restriction; newer-ABI rules trimmed rather than failing the ruleset |
| eBPF | `CAP_BPF`+BTF+`execd-ebpf` build | Skip observation; no audit file; default build reports `unsupported` |
| (any) under gVisor/Kata | host kernel exposes the syscall | Intercepted/absent syscall → that feature skipped |

**PID 1 is the design premise.** Security-critical hardening (cap-drop/seccomp/
Landlock) is per-child and does **not** depend on PID 1, so it holds even when execd
runs as a subreaper — whether by misconfiguration on the direct paths, or on the K8s
Pool path before its entrypoint is restructured (§1). In subreaper mode only
full-tree reaping and the kernel signal shield are lost, and we do not rebuild them;
`init_mode` on the capabilities endpoint reports which mode is actually in effect.

Probing extends `isolation.Probe` (which already carries a diagnostic `Message`
field populated on failure, e.g. *"bwrap not found: … searched …"*) and is reported
on `GET /v1/isolated/capabilities` under a `hardening` object so callers see what is
*actually* enforced. Each layer reports:

- a `state`: `active` | `disabled` (not configured) | `degraded` (configured but a
  prerequisite was missing, fail-open) | `unsupported` (kernel/build cannot provide
  it) — enforcement, not mere availability; and
- a **`message`** whenever `state` is not `active`, giving the concrete reason the
  layer could not be applied (which capability/syscall/kernel version was missing),
  so an operator can act on it. The same reason is also logged (as the existing probe
  does), so a degraded layer is never silent.

`init_mode` is one of `pid1` | `subreaper` | `none`; each layer's `state` is one of
`active` | `disabled` | `degraded` | `unsupported`.

```json
{ "hardening": {
    "init_mode": "pid1",
    "signal_shield": true,
    "cap_drop":  { "state": "active" },
    "seccomp":   { "state": "active" },
    "landlock":  { "state": "degraded",
                   "message": "landlock unsupported: kernel ABI < 1 (needs >= 5.13); FS confinement skipped" },
    "ebpf":      { "state": "disabled",
                   "message": "not enabled (requires the execd-ebpf build and CAP_BPF)" } } }
```

(A future operator-set `require=[…]` could turn "unavailable" into a startup error;
deferred — the default is always fail-open.)

### Configuration

The hardening settings extend execd's **existing** isolation TOML
(`--isolation-config` / `EXECD_ISOLATION_CONFIG`; `isolation/config.go`;
`configs/isolation.example.toml`) — no new mechanism.

- **Consumer**: only execd (`main.go` → `isolation.LoadConfig`). Absent file →
  built-in defaults (seccomp already on).
- **Producer**: the operator/image builder — a **static, node/image-level policy**
  (like today's `isolation.example.toml`), consistent with OSEP-0004's
  infrastructure-level, SDK-transparent stance. Not per-sandbox, not in
  `CreateSandboxRequest`.
- **Injection**: travels with execd via the existing path (Docker copy / K8s
  `execd-installer` volume); execd is pointed at it via the flag/env already set in
  `bootstrap.sh`.

This is distinct from the two existing privilege surfaces: the **container ceiling**
(Docker `drop_capabilities` / K8s `securityContext`) sets *how much execd gets*; this
TOML sets *how much execd subtracts* for user code.

**Convention over configuration.** The config surface is deliberately small: three
on/off switches plus a few security-relevant overrides. Everything else is a
built-in default (below), not a knob — operators decide *whether* to harden, not the
mechanics of it.

```toml
# The entire new surface. All three default OFF; omit the file → today's behavior.
[hardening]
enabled = false     # master switch: run user code through the reduced floor
                    #   (init + cap-drop + no_new_privs + seccomp). Sensible when on.

[landlock]
enabled = false      # add filesystem confinement on top of [hardening]
extra_writable = []  # optional: writable paths beyond the built-in set
extra_readable = []  # optional: read-only paths beyond the built-in set

[ebpf]
enabled = false     # exec/connect/privilege audit → JSONL file (needs execd-ebpf build)
```

Custom seccomp is **not** a new knob — it reuses the already-existing
`[seccomp] deny = […]` in the same TOML (`SeccompOverride`); absent → the built-in
denylist.

**Built-in defaults (not exposed as config), applied when the relevant switch is on:**

| Behavior | Built-in value | Rationale |
|---|---|---|
| init / PID 1 | on with `[hardening]` (topology via `EXECD_INIT`, §1) | init and hardening go together; no separate `[init]` toggle |
| dropped capabilities | **all** (workload keeps none) | safest default; raising the ceiling is the operator's separate call |
| `no_new_privs` | always set | free, no reason to disable |
| forwarded signals | `HUP,USR1,USR2,WINCH` | the set apps actually need; not worth exposing |
| Landlock read/exec paths | system paths + `/proc/self` + read `/run` (+ `extra_readable`) | run real programs without exposing other PIDs' procfs (§5) |
| Landlock read/write paths | needed `/dev` files + `/tmp`,`/run` + `allowed_writable` (`/workspace`,`/mnt`,`/media`,`/data`) + `extra_writable` | reuse the writable list already in this TOML |
| eBPF observed events | `exec,connect,privilege` | the audit-relevant set |
| eBPF audit file | `/var/log/opensandbox/ebpf-audit.jsonl` (rotated) | fixed path; shipping off-node is the operator's concern |

So the common cases are one line each: `[hardening] enabled = true` to turn on the
floor; add `[landlock] enabled = true` for FS confinement; add `[ebpf] enabled =
true` for audit. Fine-grained needs (custom seccomp denylist, extra writable paths)
have exactly one override each; the rest is convention.

**Full configuration reference.** Every key execd reads from the isolation TOML —
the pre-existing isolation fields plus this OSEP's additions. All keys are optional;
an absent file or absent key uses the default.

| Key | Type | Default | Since | Purpose |
|---|---|---|---|---|
| `upper_root` | string | `/var/lib/execd/isolation` | existing | Parent dir for per-session overlay upper dirs |
| `upper_max_bytes` | int | `8589934592` (8 GiB) | existing | Cap on total upper size across sessions |
| `diff_max_bytes` | int | `4294967296` (4 GiB) | existing | Cap on tar.gz diff output size |
| `allowed_writable` | []string | `["/workspace","/mnt","/media","/data"]` | existing | Host paths callers may bind writable (replaces default, no merge; also the Landlock read/write base) |
| `[seccomp] deny` | []string | *(absent → built-in denylist)* | existing | Replaces the built-in syscall denylist; also the workload seccomp floor when `[hardening]` is on. The launcher's exec syscall (`execve`) is rejected at load time (would deny its final exec, §4); `execveat` stays allowed |
| `[hardening] enabled` | bool | `false` | new | Master switch: run all user code through the reduced floor (init + cap-drop + `no_new_privs` + seccomp). Paired with `EXECD_INIT` (§1) |
| `[hardening] keep_capabilities` | []string | `[]` (drop all) | new | Capabilities the workload retains (raised in the ambient set, §4); default keeps none |
| `[landlock] enabled` | bool | `false` | new | Add filesystem confinement on top of `[hardening]` |
| `[landlock] extra_writable` | []string | `[]` | new | Writable paths beyond the built-in set (`/dev` files, `/tmp`, `/run`, `allowed_writable`) |
| `[landlock] extra_readable` | []string | `[]` | new | Read-only paths beyond the built-in set (system paths, `/proc/self`) |
| `[ebpf] enabled` | bool | `false` | new | Enable exec/connect/privilege audit (requires the `execd-ebpf` build + `CAP_BPF`) |
| `[ebpf] observe` | []string | `["exec","connect","privilege"]` | new | Which event kinds to record |
| `[ebpf] audit_file` | string | `/var/log/opensandbox/ebpf-audit.jsonl` | new | Append-only JSONL audit sink (rotated) |

Notes: `no_new_privs`, forwarded signals (`HUP,USR1,USR2,WINCH`), the Landlock
system/`/proc/self` read set, and the essential device-file writes are **not** keys —
they are built-in defaults applied when the relevant switch is on (rationale in the
table above). Topology (whether execd becomes PID 1) is driven by the
`EXECD_INIT` env var in lockstep with `[hardening] enabled`, not by a TOML key (§1).

Recommended container ceiling (separate operator surface) keeps what execd needs and
drops the rest, e.g. Docker `drop_capabilities =
["NET_RAW","SYS_MODULE","SYS_TIME","SYS_TTY_CONFIG","AUDIT_WRITE","MKNOD"]` (leave
`CAP_SETPCAP`/`CAP_SETUID`/`CAP_SETGID` so execd can reduce children).

## Test Plan

**Unit** — single reaper dispatches owned-child status to the registered waiter and
reaps orphans (no `Cmd.Wait`/`wait4` race, no lost exit codes); signal shield
(workload `kill 1`/`kill -9 1` inert); prelude order (KEEPCAPS → trim bounding set →
identity change → clear caps; a bare uid change or early cap clear would `EPERM`);
`KeepCaps` survives `execve` via the ambient set; cap drop hits child not execd;
seccomp generator parity with bwrap; `[seccomp] deny` containing `execve` is rejected
at load time while `execveat`-only stays valid; Landlock ABI probe + trimming; panic isolation;
config parses with safe defaults; missing-`CAP_SETPCAP` and independent-layer-skip
degradations; capabilities endpoint reflects real state; **same-UID workload cannot
read execd's env (`/proc/1/environ` denied) nor drive the trusted-stop channel**.

**Integration** — PID 1 handoff after `exec` (and the subreaper misconfig path); no
zombie accumulation over N cycles; layering invariant (execd at ceiling, workload
reduced); **every user-code path reduced** (`/command`, `/code`, PTY, entrypoint,
isolated session); Landlock enforcement; fail-open under gVisor; workload `kill 1` is
inert; trusted out-of-band stop cleanly recycles; the K8s `kill 1` recycle path
behaves as reconciled per §3 (across Docker and containerd/gVisor).

**E2E** — default image unchanged with hardening off; eBPF variant writes the JSONL
audit file; long-running fork-heavy sandbox keeps a bounded process table.

## Drawbacks

1. **execd is a single point of failure** as PID 1; crash recovery relies on the
   outer runtime (crashes only — normal stop uses the trusted out-of-band channel of
   §3, not in-namespace `kill 1`, which init-mode execd ignores).
2. **Larger TCB in one process** — init shares an address space with
   HTTP/Jupyter/isolation; needs disciplined panic isolation.
3. **Security-critical complexity** — fork/exec ordering, reaper races, covering
   every launch path.
4. **Operator responsibility** — layering needs a ceiling that grants execd
   `CAP_SETPCAP` etc.; too tight → degraded (probed + reported).
5. **Kernel/runtime variance** for Landlock/eBPF; **eBPF build cost** (CGO variant).
6. **Workload breakage risk** from any restriction — conservative defaults + opt-in.

## Alternatives

1. **Separate supervisor as PID 1, execd as worker** (`opensandbox-supervisor`).
   Pro: a minimal init can restart execd on crash. Con: adds a process + a
   signal-forwarding hop, and does **not** remove the outer restarter — the
   kubelet/Docker `restartPolicy` is already the ultimate restarter for a crashed
   PID 1, so the marginal benefit is small. **Rejected**; crash isolation is instead
   handled in-process (§3). It remains the fallback if crash isolation later
   outweighs process minimalism (execd composes cleanly as a worker).
2. **tini/dumb-init as PID 1.** Solves only reaping/signals; no privilege layering,
   seccomp, Landlock, or eBPF, and still forwards `kill 1` to execd. **Rejected** as
   a sole solution; its reaping algorithm is the reference for execd's reaper.
3. **Bubblewrap the whole sandbox.** Wrapping the long-lived control plane in bwrap
   complicates lifecycle/PTY/secure-runtime compatibility; bwrap targets transient
   jails. **Rejected**; bwrap stays scoped to isolated sessions under the floor.
4. **Do nothing.** Leaves the reaping, privilege-parity, and audit gaps. **Rejected.**

## Open Implementation Questions

These are settled during implementation, not in this draft. They are recorded so the
design intent is unambiguous while the exact mechanism is left to the implementing PR.

1. **Single authoritative enable switch driving both topology and floor.** `[hardening]
   enabled` (TOML, read by execd) and `EXECD_INIT` (env, read by `bootstrap.sh` before
   execd starts) must not drift: enabling one without the other either skips the floor
   or fails to become PID 1. The server sets both from one source of truth; the concrete
   wiring (single server-side flag that emits both, or deriving `EXECD_INIT` from the
   rendered TOML at inject time) is an implementation decision. Until unified, treat the
   env var as authoritative for topology and require the server to keep them in lockstep.

2. **External SIGTERM must be forwarded to the entrypoint.** The forwarded-signal set
   must include **SIGTERM** for runtime-initiated container stop (Docker/K8s send it to
   PID 1), matching today's `bootstrap.sh:333` and the `sigterm_forward.sh` test — so the
   workload gets graceful shutdown before SIGKILL. This is distinct from §3: *external*
   SIGTERM (runtime, out-of-namespace) is forwarded/acted on; *in-namespace* workload
   SIGTERM is still ignored. Distinguishing the two reliably is part of the §3 trusted-
   stop open item.

3. **Process management cannot wrap `os/exec` and reuse its cleanup.** Go keeps
   `Cmd.awaitGoroutines`, the pipe list, and `os.ProcessState` internals private, and a
   second `Wait` after the central `wait4` returns `ECHILD`. So the managed-process
   abstraction must **own** the child's lifecycle directly (create pipes, run the I/O
   copy goroutines, build the status from the reaper-delivered `WaitStatus`) rather than
   delegating teardown to `os/exec`. The native launcher (§4) already implies a
   non-`os/exec` launch path; the reaper feeds it statuses.

4. **Landlock must allow writes to essential device files.** `/dev/null`, `/dev/zero`,
   and the controlling terminal need **write**, not just read/exec, or `cmd >/dev/null`
   and normal tty output fail. The default policy grants narrowly scoped writable device
   rules for the common device files rather than making all of `/dev` writable; the exact
   device list is refined against real images.

5. **Landlock `/proc/self` vs forked descendants.** A `PATH_BENEATH` rule built from
   `/proc/self` is inode-based (the launcher's `/proc/<pid>`); the initial workload keeps
   that PID across `execve`, but a *forked descendant*'s `/proc/self` resolves to a
   different directory the inherited ruleset does not allow, so descendants that inspect
   their own procfs would fail. Granting all of `/proc` is not an option (it would re-expose
   `/proc/1`, §3). The mechanism that gives each descendant access to *its own* procfs
   without exposing other PIDs (e.g. a bind of a per-process procfs view, or accepting the
   limitation for `/proc/self`-dependent tooling) is resolved at implementation time.

## Upgrade & Migration Strategy

**Backward compatible.** With `[hardening]`/`[landlock]`/`[ebpf]` disabled and
`EXECD_INIT` unset, execd behaves as today (`bootstrap.sh` owns the tree; execd is
HTTP-only). No CRD or `CreateSandboxRequest` changes; no new config mechanism (new
sections in the existing isolation TOML); execd keeps full privilege, only user code
is reduced.

**One additive API surface (not breaking).** Reporting enforcement state adds a
`hardening` object to `GET /v1/isolated/capabilities`, so the public
`CapabilitiesResponse` in `specs/execd-api.yaml` and the SDK models
(`sdks/sandbox/go/isolated.go`, `sdks/sandbox/python/.../models/isolated.py`) gain
that optional field. This is a backward-compatible *addition* — existing callers are
unaffected and existing fields are unchanged — but per the repo's spec/SDK-alignment
rule the spec and generated SDK models must be updated together when this ships.

**Phased rollout, each shippable and reversible via config:**

1. execd `--init` mode: own+reap the tree (single reaper + status dispatch), forward
   signals, PID 1 signal shield, trusted out-of-band stop, self-resilience;
   `bootstrap.sh` `exec`s into execd (`EXECD_INIT`). **Reconcile the K8s `kill 1`
   recycle path (§3) before enabling init mode there.** No workload hardening yet.
2. The pre-exec floor behind `[hardening] enabled` (init + `no_new_privs` + cap drop
   + seccomp reuse, via the native launcher); document the recommended ceiling.
3. Landlock behind `[landlock] enabled`.
4. Opt-in eBPF observation (`execd-ebpf` build variant, `[ebpf] enabled`).
