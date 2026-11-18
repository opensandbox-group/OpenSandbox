---
title: In-Sandbox Process QoS and Saturation Diagnostics
authors:
  - "@hittyt"
creation-date: 2026-11-17
last-updated: 2026-11-18
status: draft
---

# OSEP-0031: In-Sandbox Process QoS and Saturation Diagnostics

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Architecture](#architecture)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [1. Two-Tier QoS Model and Zero Role Assumptions](#1-two-tier-qos-model-and-zero-role-assumptions)
  - [2. Static Opt-In Surface (TOML and Environment)](#2-static-opt-in-surface-toml-and-environment)
  - [3. Dynamic Per-Launch QoS (`POST /pty`, `POST /command`, `POST /session`, Lifecycle Hooks)](#3-dynamic-per-launch-qos-post-pty-post-command-post-session-lifecycle-hooks)
  - [4. Enforcement Mechanics: L1 Scheduler/OOM Floor and L2 Cgroup v2 Split](#4-enforcement-mechanics-l1-scheduleroom-floor-and-l2-cgroup-v2-split)
  - [5. Orthogonality with OSEP-0018 Privilege Hardening](#5-orthogonality-with-osep-0018-privilege-hardening)
  - [6. Non-Blocking Saturation and I/O-Stall Diagnostics (`GET /metrics`)](#6-non-blocking-saturation-and-io-stall-diagnostics-get-metrics)
  - [7. Capability Probing and Degradation Reporting](#7-capability-probing-and-degradation-reporting)
  - [8. Relationship to Existing Proposals](#8-relationship-to-existing-proposals)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Building on `execd` as the sandbox init ([OSEP-0018](0018-execd-as-sandbox-init.md)), this proposal introduces an opt-in, two-tier in-sandbox Quality of Service (QoS) model (`high` vs. `standard`) and non-blocking kernel pressure diagnostics on `GET /metrics`. Only `execd` itself defaults to the `high` tier; every child process starts in the `standard` workload tier (unprivileged scheduler priority reduction and `oom_score_adj` victim steering via `opensandbox-launcher`, plus optional Cgroup v2 `system`/`workload` subtree isolation) unless explicitly promoted via static configuration or a per-request `qos: "high"` field on `POST /pty`, `POST /command`, or `POST /session`. This keeps `execd`, creator-designated platform daemons (such as in-sandbox `mitmproxy` or control agents), and emergency WebShell/command sessions responsive under 100% CPU load and protected from collateral OOM kills during workload memory spikes, while letting operators immediately distinguish CPU throttling, storage I/O hangs, and host/runtime failures without adding bespoke process-management APIs or server-side switches.

## Motivation

[OSEP-0018](0018-execd-as-sandbox-init.md) consolidated process-tree ownership under `execd` (PID 1 / subreaper) and routed child launches through `launchManaged` and `opensandbox-launcher`. However, resource accounting, CPU scheduling, and OOM victim selection inside a sandbox remain completely flat: `execd`, in-sandbox platform daemons (e.g., `mitmproxy`, MCP/browser services, health agents), interactive WebShells (`/pty`), and untrusted user workloads all share the same sandbox cgroup, run at `SCHED_OTHER` with `nice = 0`, and inherit identical `oom_score_adj` values.

In production multi-tenant and platform-hosted sandboxes, this flat model causes four recurring operational pain points:

1. **CPU saturation locks out WebShell and starves platform daemons.** A single user build, training script, or tight loop (`make -j`, OpenMP/PyTorch worker pools) saturates the container's CFS CPU quota. Because `execd`'s HTTP/WebSocket runtime, platform daemons running inside the sandbox, and newly spawned `/pty` shells compete at equal weight behind dozens of runnable user threads, WebShell login (`POST /pty` + `GET /pty/{sessionId}/ws`) and in-sandbox control tasks stall or time out.
2. **Workload memory spikes trigger collateral OOM kills of core platform daemons.** When user code exhausts sandbox memory under a uniform `oom_score_adj`, Linux's `oom_badness()` frequently kills essential in-sandbox daemons (such as `mitmproxy` or `execd`) instead of the offending workload for two kernel reasons: (a) under `oom_score_adj = 0`, if a workload forks many moderate-RSS workers whose combined RSS trips the limit, a single resident daemon with a larger per-process RSS scores highest and dies first; (b) in Kubernetes `Guaranteed` pods (`requests.memory == limits.memory`), kubelet sets container `oom_score_adj = -997`, subtracting 99.7% of total memory from every task's badness and clamping both a 300 MiB daemon and a multi-gigabyte memory bomb to the floor score of `1` — causing `select_bad_process()` (which only replaces the candidate on a strictly higher score) to kill the earliest-spawned process in the cgroup.
3. **CPU exhaustion, I/O hangs, and dead hosts look identical from outside.** When a sandbox stops responding, `GET /metrics` currently sleeps synchronously for 1 second (`resources.CPUPercent(time.Second)`) and exposes only aggregate CPU and memory percentages. Operators cannot tell whether `execd` timed out because the host/VM died, whether the sandbox is healthy but throttled at 100% CPU, or whether processes are wedged in uninterruptible sleep (`D` state) on a hung network/FUSE volume.
4. **Runtime-specific escape hatches fragment operations.** Kubernetes deployments often fall back to `kubectl exec` for emergency troubleshooting, which does not exist on Fast Sandbox (Firecracker microVMs) or standalone Docker hosts. Unifying emergency WebShell and remediation (`ps`, `kill`) onto `execd`'s `/pty` and `/command` routes requires `execd` to guarantee a high-priority execution lane across every OpenSandbox runtime.

### Goals

1. **Guaranteed control-plane, daemon, and emergency WebShell survival:** Keep `execd` and `qos: "high"` processes (`ENTRYPOINT`/lifecycle daemons such as `mitmproxy`, plus emergency `/pty`, `/command`, and `/session` executions) responsive under 100% CPU saturation and shielded from collateral OOM kills when `standard` workloads spike memory, while remaining work-conserving when idle.
2. **Zero hardcoded role assumptions:** Make no built-in assumption about whether `ENTRYPOINT` or lifecycle hooks are "platform" or "user" code; default all child processes to `standard` when QoS is enabled and let sandbox creators or API callers explicitly select `high`.
3. **Two-level progressive enforcement:** Provide a universal, zero-privilege L1 floor (`nice` / `SCHED_IDLE` + `RLIMIT_NICE=0` + `oom_score_adj` ratcheting) that works across Docker, Kubernetes, gVisor, Kata, and Fast Sandbox, plus an opt-in L2 Cgroup v2 subtree split (`system` vs. `workload`) for group-level CPU weight/quota, hard workload OOM containment, and PID reservation where writable Cgroup v2 is available.
4. **Orthogonality with privilege hardening:** Keep `[qos]` independent of OSEP-0018 `[hardening]`, and ensure `qos: "high"` never bypasses capability, seccomp, or Landlock restrictions.
5. **Sub-millisecond saturation vs. I/O-hang triage:** Make `GET /metrics` non-blocking and expose kernel CPU-throttling counters, PSI (`cpu`, `io`, `memory`), and runnable/uninterruptible (`D`-state) process counts read strictly from pseudo-filesystems.
6. **Formalize `/pty` in the OpenAPI contract:** Document the existing `/pty` endpoints alongside the new `qos` request field in `specs/execd-api.yaml`.

### Non-Goals

1. **New process-management REST endpoints:** Listing and terminating runaway processes (`ps`, `top`, `kill`) are performed through `qos: "high"` `/pty` and `/command` sessions rather than introducing bespoke `/v1/processes` routes.
2. **Platform-enforced server switches or separate admin tokens:** Like OSEP-0018 `[hardening]`, QoS is creator-opted via in-sandbox TOML or environment variables; no `[runtime]` server config flag or secondary authentication token is introduced in this proposal.
3. **Disk bandwidth (`io.max`) throttling:** Identifying I/O stalls is in scope; block-device IOPS/BPS throttling (which requires host major:minor device mapping) is out of scope.
4. **Windows QoS enforcement:** Enforcement and `/proc` / cgroup pressure metrics target Linux sandboxes; non-Linux builds report `unsupported` and treat `qos` fields as no-ops.

## Requirements

| ID | Requirement | Priority |
|---|---|---|
| R1 | When QoS is disabled (the default), process scheduling, OOM scores, cgroup placement, and resource limits are unchanged from today | Must |
| R2 | When QoS is enabled, `execd` stays in `high` and all spawned child processes (`ENTRYPOINT`, lifecycle hooks, `/command`, `/session`, `/pty`, isolated sessions) default to `standard` unless explicitly configured or requested as `high` | Must |
| R3 | L1 (`nice` / `SCHED_IDLE` + `RLIMIT_NICE=0` + `oom_score_adj`) requires no extra container capabilities (`CAP_SYS_NICE`, `CAP_SYS_RESOURCE`, `CAP_SYS_ADMIN`), works across UID transitions, and prevents `standard` children without those capabilities from raising their CPU priority or lowering their `oom_score_adj` back | Must |
| R4 | `POST /pty`, `POST /command`, and `POST /session` accept an optional `qos` (`"standard" \| "high"`) field; `qos: "high"` succeeds as a benign no-op when `[qos]` is disabled so unified WebShell clients need not pre-probe sandbox config | Must |
| R5 | `[qos]` and `[hardening]` operate orthogonally: `[qos]` works with `[hardening]` off, and `qos: "high"` never bypasses `[hardening]` capability drops, `no_new_privs`, seccomp, or Landlock | Must |
| R6 | L2 Cgroup v2 split (`system/` vs. `workload/`) is opt-in, migrates existing root PIDs (including `execd` and microVM guest `/init`) to satisfy the Cgroup v2 no-internal-processes rule, and fails open to L1 when unavailable | Should |
| R7 | `GET /metrics` responds without blocking sleeps and exposes optional CPU throttle counters, PSI stall averages, and `R`/`D`-state task counts using only `/proc` and `/sys/fs/cgroup` reads (zero disk/mount `stat` calls) | Must |
| R8 | Startup QoS enforcement state (`active` / `disabled` / `degraded` / `unsupported` + diagnostic `message`) is reported on `GET /v1/isolated/capabilities` | Must |

## Proposal

### Architecture

```text
Sandbox root (container cgroup or microVM guest)
├── [high tier]   nice = 0 · baseline oom_score_adj (e.g. 0 or K8s Guaranteed -997)
│   │             + (optional) cgroup: system/ (cpu.weight=1000, PID headroom)
│   ├── execd (PID 1 / subreaper)  ── always high; serves /ping, /metrics (<1ms), /pty, /command
│   ├── (optional) guest /init     ── migrated into system/ on Fast Sandbox microVMs
│   ├── (opt-in) ENTRYPOINT / lifecycle daemons (e.g. mitmproxy) configured with qos = "high"
│   └── (per-call) POST /pty, /command, /session with {"qos": "high"}  ← Emergency WebShell & ops
│
└── [standard tier] (default for all spawned children when [qos] enabled)
    │   via opensandbox-launcher: RLIMIT_NICE=0 + nice=19 / SCHED_IDLE + oom_score_adj=max(cur,500)
    │   + (optional) cgroup: workload/ (cpu.weight=50, optional cpu.max / memory.max / pids.max)
    ├── ENTRYPOINT / lifecycle hooks (default qos = "standard")
    ├── POST /command, /session, /pty (default qos = "standard")
    ├── /code (Jupyter server & kernels inherit entrypoint's tier)
    └── /v1/isolated/session (bwrap tree inherits standard tier + session isolation)
```

### Notes/Constraints/Caveats

- **Why L2 Cgroup v2 split is opt-in separately from L1 (`[qos.cgroup] enabled = false`):**
  1. Standard Docker and Kubernetes containers mount `/sys/fs/cgroup` read-only and drop `CAP_SYS_ADMIN`, so creating sub-cgroups inside the container is rejected by the kernel.
  2. Even when `/sys/fs/cgroup` is writable inside an OCI container, enabling domain controllers (`+cpu +memory +pids`) on the container's root cgroup triggers the Linux Cgroup v2 *no internal processes* constraint: host-initiated `docker exec` / `kubectl exec` calls that attempt to attach a process to the container's root `cgroup.procs` fail with `EBUSY` (affecting external `kubectl exec` workflows and the Kubernetes Pool `Restart` recycle `pod-exec kill 1` path).
  3. Conversely, in **Fast Sandbox (Firecracker microVMs)** — or dedicated Cgroup-delegated environments standardized on `execd` `/pty` — `[qos.cgroup] enabled = true` works cleanly and adds group-level CPU fairness (immune to thousand-thread dilution), workload-scoped OOM (`workload/memory.max` never kills `execd` or platform daemons), and reserved PID capacity (`workload/pids.max`).
- **Kubernetes `Burstable` pod ceiling on L1 `oom_score_adj`:** In Docker (`oom_score_adj = 0`) and Kubernetes `Guaranteed` pods (`oom_score_adj = -997`), raising `standard` workloads to `+500` creates a `500`–`1497` point badness gap (equivalent to 50%–149.7% of sandbox RAM) with zero extra capabilities. However, in Kubernetes `Burstable` pods (`requests.memory < limits.memory`), kubelet initializes container `oom_score_adj` based on the request-to-node-capacity ratio — often `+900`..`+990` on large nodes — leaving narrow headroom below the `+1000` ceiling when `CAP_SYS_RESOURCE` is absent. Deployments relying on L1 OOM victim steering in Kubernetes should therefore size sandboxes with `requests.memory == limits.memory` (`Guaranteed`) or enable L2 `workload_memory_max`.
- **Why `RLIMIT_NPROC` is not set by L1:** Linux accounts `RLIMIT_NPROC` per real UID across the user namespace rather than per process subtree. When `execd`, a platform daemon, and a workload share the same UID (e.g., `root` or `sandbox`), a low `RLIMIT_NPROC` on `standard` children would equally block `qos: "high"` `/pty` shells running under that UID from forking `ps`. Subtree PID reservation is therefore handled strictly by L2 `workload/pids.max`.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Thousand-thread fork storm under L1-only (`nice = 19`) dilutes single-threaded `nice = 0` share | Support `scheduler = "idle"` (`SCHED_IDLE`, weight `3` vs. `1024`, ~341:1 per thread) in L1, and L2 `workload/cpu.weight` + `workload/pids.max` where Cgroup v2 is writable |
| Non-root UID transition clears `dumpable`, blocking post-`setuid` `open("/proc/self/oom_score_adj")` with `EACCES` | `opensandbox-launcher` opens `/proc/self/oom_score_adj` (`O_RDWR \| O_CLOEXEC`) *before* `setuid` while `dumpable == 1`, and writes the raised score *after* `setuid` and `capset` (§4) |
| Caller with `X-EXECD-ACCESS-TOKEN` passes `qos: "high"` to escape security restrictions | `qos` controls scheduling/OOM/cgroup placement only; `[hardening]` cap-drop, `no_new_privs`, seccomp, and Landlock apply identically to `high` and `standard` launches (§5) |
| Hung workspace volume (`/workspace` on wedged NFS/FUSE) blocks `GET /metrics` or emergency WebShell | `GET /metrics` reads only `/proc` and `/sys/fs/cgroup`; emergency `/pty` and `/command` callers can pass `"cwd": "/"` and `"command": "/bin/sh --noprofile --norc"` to avoid touching hung mounts |
| Concurrent `GET /metrics` and OTel scrapes reset each other's CPU delta window | Replace ad-hoc per-caller baseline mutation with a shared 1-second rolling sampler in `pkg/resources` (§6) |

## Design Details

### 1. Two-Tier QoS Model and Zero Role Assumptions

Every process in the sandbox belongs to one of two QoS classes:

- **`high`** — Retains `execd`'s baseline scheduling priority (`SCHED_OTHER`, `nice = 0`) and baseline `oom_score_adj`, and, when L2 is active, resides in the `system` sub-cgroup.
- **`standard`** — Runs with reduced scheduling priority (`RLIMIT_NICE = 0` plus `nice = 19` or `SCHED_IDLE`), raised OOM victim badness (`oom_score_adj = max(current, configured)`, default `+500`), and, when L2 is active, resides in the `workload` sub-cgroup.

**No implicit privilege for `ENTRYPOINT`:**
- `execd` itself (and, on Fast Sandbox microVMs under L2, the guest `/init` process) is always placed in `high`.
- When `[qos]` is enabled, **every** child launched by `execd` — including the image `ENTRYPOINT` and `lifecycle` hooks — defaults to `standard`. For general-purpose sandboxes whose `ENTRYPOINT` runs user application code or Jupyter kernels, this ensures a runaway `ENTRYPOINT` cannot starve `execd`, block an emergency `qos: "high"` WebShell, or tie `execd`'s OOM score.
- Platform builders whose `ENTRYPOINT` or `lifecycle` hook runs a trusted in-sandbox daemon (for example, an in-sandbox `mitmproxy`, credential agent, or workspace supervisor) explicitly opt that target into `high` via configuration (§2, §3).

### 2. Static Opt-In Surface (TOML and Environment)

Following [OSEP-0018](0018-execd-as-sandbox-init.md), QoS is configured through the existing `execd` isolation TOML (`EXECD_ISOLATION_CONFIG`, `pkg/isolation/config.go`) so all in-sandbox runtime policies live in one file. For creators who want L1 protection on arbitrary third-party images without baking or mounting a TOML file, two environment variables provide a zero-file shorthand.

#### TOML schema (`[qos]` and `[qos.cgroup]`)

```toml
# Process QoS tiering (OSEP-0031). Default OFF — omit for today's flat behavior.
[qos]
enabled = true              # master switch for L1 scheduler + OOM tiering (default: false)
entrypoint = "standard"     # QoS tier for image ENTRYPOINT: "standard" | "high" (default: "standard")
lifecycle = "standard"      # default QoS tier for lifecycle hooks: "standard" | "high" (default: "standard")
nice = 19                   # nice value (1..19) for "standard" when scheduler = "other" (default: 19)
scheduler = "other"         # "other" (SCHED_OTHER + nice) | "idle" (SCHED_IDLE) (default: "other")
oom_score_adj = 500         # /proc/self/oom_score_adj floor (0..1000) for "standard"; 0 disables (default: 500)

# Optional L2 Cgroup v2 subtree isolation on top of [qos]. Default OFF.
# Recommended on Fast Sandbox microVMs or Cgroup-delegated pods using execd /pty.
[qos.cgroup]
enabled = false             # split sandbox cgroup into system/ and workload/ (fail-open to L1)
system_cpu_weight = 1000    # cpu.weight (1..10000) for high tier (default: 1000)
workload_cpu_weight = 50    # cpu.weight (1..10000) for standard tier (default: 50)
workload_cpu_max_pct = 0    # optional hard cap as % of sandbox CPU capacity, 1..100 (0 = uncapped)
workload_memory_max = ""    # optional memory.max for workload/, e.g. "90%" or "3584Mi" ("" = uncapped)
workload_pids_max = 0       # optional pids.max for workload/, >0 caps workload tasks (0 = uncapped)
```

Validation rules (invalid static configuration fails `execd` startup fast, matching OSEP-0018):
- `entrypoint` and `lifecycle` must be `"standard"` or `"high"`.
- `nice` must be in `1..19` when `enabled = true` (omitted in TOML defaults to `19`).
- `scheduler` must be `"other"` or `"idle"`.
- `oom_score_adj` must be in `0..1000` (omitted in TOML defaults to `500`; an explicit `0` disables L1 OOM score adjustment).
- `[qos.cgroup] enabled = true` requires `[qos] enabled = true`. `system_cpu_weight` and `workload_cpu_weight` must be in `1..10000`; `workload_cpu_max_pct` in `0..100`; `workload_memory_max` accepts byte quantities (`"3584Mi"`, `"4Gi"`) or a percentage (`"1%"`..`"99%"`) of the detected sandbox memory limit (if no finite sandbox/host memory limit exists, a percentage value logs a warning and degrades `memory` to uncapped).

#### Lightweight environment overrides

| Variable | Values | Effect |
|---|---|---|
| `EXECD_QOS_ENABLED` | `true` / `1` / `false` / `0` | Overrides `[qos] enabled`. Setting `EXECD_QOS_ENABLED=true` in `CreateSandboxRequest.env` activates L1 QoS with built-in defaults (`nice = 19`, `oom_score_adj = 500`, `entrypoint = "standard"`) with no TOML file required. |
| `EXECD_QOS_ENTRYPOINT` | `standard` / `high` | Overrides `[qos] entrypoint`. |

Both variable names are added to `execdConfigEnvBlacklist` (`pkg/isolation/config.go`) and `bootstrapStripEnv` (`pkg/runtime/initmode_linux.go`) so they are stripped before user code executes.

### 3. Dynamic Per-Launch QoS (`POST /pty`, `POST /command`, `POST /session`, Lifecycle Hooks)

#### Execution API additions (`specs/execd-api.yaml`)

Add a shared schema `ProcessQoS`:

```yaml
ProcessQoS:
  type: string
  enum: [standard, high]
  default: standard
  description: >-
    In-sandbox scheduling and resource QoS tier (OSEP-0031). "standard" places
    the process in the managed workload tier when [qos] is enabled. "high"
    keeps the process in the control-plane tier alongside execd (for example,
    emergency WebShell login or platform daemons such as mitmproxy). Accepted
    as a no-op when [qos] is disabled; never bypasses [hardening] security controls.
```

Wire `qos` onto the three interactive/command creation requests:

1. **`POST /pty` (`CreatePTYSessionRequest`)** — Formalize `/pty`, `/pty/{sessionId}`, and `/pty/{sessionId}/ws` in `specs/execd-api.yaml` and add `qos`:
   ```json
   {
     "cwd": "/",
     "command": "/bin/bash",
     "qos": "high"
   }
   ```
   Console BFF (`console/bff/app/shell.py`) and platform WebShell gateways pass `"qos": "high"` when opening operator/emergency WebShell sessions. Because `"qos": "high"` is a valid no-op on sandboxes where `[qos]` is off, WebShell clients can send it unconditionally without pre-probing sandbox capabilities.
2. **`POST /command` (`RunCommandRequest`)** — Add optional `"qos": "standard" | "high"`. Platform control planes can launch resident helper daemons (`background: true, qos: "high"`) or run non-interactive inspection (`ps -eo pid,ppid,stat,pcpu,rss,comm,wchan`) and remediation (`kill -9 <pid>`) in the `high` tier even under 100% workload CPU load.
3. **`POST /session` (`CreateSessionRequest`)** — Add optional `"qos": "standard" | "high"`, governing the persistent shell process and every `run_in_session` child it forks.

Unknown `qos` values (anything other than `""`, `"standard"`, or `"high"`) are rejected with HTTP `400 Bad Request`.

#### Lifecycle hook schema (`pkg/lifecycle/config.go`)

Extend `Hook` and `PeriodicHook` with an optional `qos` field (`"standard" | "high"`):

```json
{
  "version": 1,
  "preStart": { "command": ["/opt/platform/mitmdump-wrapper.sh"], "qos": "high" },
  "periodic": [
    { "name": "watchdog", "schedule": "@every 30s", "command": ["/opt/platform/check.sh"], "qos": "high" }
  ]
}
```

When omitted on a hook, it inherits `[qos] lifecycle` (default `"standard"`).

### 4. Enforcement Mechanics: L1 Scheduler/OOM Floor and L2 Cgroup v2 Split

#### L1: Pre-exec scheduler and OOM floor in `opensandbox-launcher`

Why apply both CPU priority and `oom_score_adj` inside `opensandbox-launcher` (`components/execd/native/launcher.c`) rather than from `execd` after `cmd.Start()`?
- **CPU priority (`sys_setpriority`):** A parent process running as UID 0 **without** `CAP_SYS_NICE` (Docker's default capability set omits `CAP_SYS_NICE`) gets `EPERM` if it calls `setpriority` on a child that already switched to a non-zero UID via `SysProcAttr.Credential`. Inside `opensandbox-launcher`, the child adjusts **itself** (`who = 0`) before `execve`, requiring zero capabilities with no race window.
- **OOM badness (`__set_oom_adj` in `fs/proc/base.c`):** Linux permits any unprivileged task to *increase* its own `/proc/self/oom_score_adj` without `CAP_SYS_RESOURCE`. Crucially, when `write()` executes while `!capable(CAP_SYS_RESOURCE)` holds (true by default in standard Docker/Kubernetes containers, and enforced after `capset` when `[hardening]` is active), the kernel also ratchets `task->signal->oom_score_adj_min` up to the written value — irrevocably preventing the workload and all its forked descendants from lowering `/proc/self/oom_score_adj` back.

Policy header extension (`launcher.c` / `hardening_linux.go`, bumping `POLICY_VERSION` to `2` — `execd` and `opensandbox-launcher` are always staged from the same `execd` artifact):
- Add `FLAG_QOS_LOW (0x4u)`, `uint8_t nice`, `uint8_t sched_policy` (`0 = SCHED_OTHER`, `1 = SCHED_IDLE`), and `uint16_t oom_score_adj` (`0..1000`) to `struct policy_header`.
- Execution sequence in `launcher.c` when `FLAG_QOS_LOW` is set (carefully ordered around `setuid`, `capset`, Landlock, and seccomp):
  1. **Pre-`setuid` descriptor acquisition:** If `hdr.oom_score_adj > 0`, open `oom_fd = open("/proc/self/oom_score_adj", O_RDWR | O_CLOEXEC)` *before* `FLAG_UID_DROP` (`setuid`). Invoking `opensandbox-launcher` via `execve` resets `mm->dumpable = 1`, so `/proc/self/oom_score_adj` is owned by the current UID; calling `setuid` later clears `dumpable` to `0` and reverts `/proc/self/*` ownership to `0:0`, which would reject a post-`setuid` `open()` as a non-root user with `EACCES`.
  2. **Post-`setuid` / post-`capset`, pre-Landlock / pre-seccomp application:**
     - `setrlimit(RLIMIT_NICE, &(struct rlimit){ .rlim_cur = 0, .rlim_max = 0 })` — pins `RLIMIT_NICE` to `0` (`nice_to_rlimit(0) = 20 > 0`), ensuring `can_nice()` denies any subsequent attempt by the workload to lower `nice` or exit `SCHED_IDLE`.
     - If `sched_policy == 1`: `sched_setscheduler(0, SCHED_IDLE, &(struct sched_param){ .sched_priority = 0 })`; else `setpriority(PRIO_PROCESS, 0, (int)hdr.nice)`.
     - If `oom_fd >= 0`: read the current integer `cur` from `oom_fd`; if `cur < (int)hdr.oom_score_adj`, `pread`/`pwrite` (or `lseek(0)` + `write`) `hdr.oom_score_adj` to `oom_fd`; `close(oom_fd)`. Clamping to `max(cur, hdr.oom_score_adj)` avoids `EACCES` when the outer runtime (e.g., a Kubernetes `BestEffort` pod at `1000`) already set a higher floor, and writing *after* `capset` ensures `oom_score_adj_min` is ratcheted whenever `CAP_SYS_RESOURCE` is absent or dropped.
  3. Failures log to stderr and fail open (closing `oom_fd` if open and never aborting workload launch).

#### L2: Opportunistic Cgroup v2 subtree split (`system/` vs. `workload/`)

When `[qos.cgroup] enabled = true`, `runtime.InitQoS` runs at `execd` startup:

1. **Discover and verify:** Resolve `execd`'s unified Cgroup v2 directory `D` via `pkg/cgroupfs`. Verify `D` is on a writable `cgroup2` mount.
2. **Create leaf sub-cgroups and vacate `D`:**
   - `mkdir(D/system, 0755)` and `mkdir(D/workload, 0755)`.
   - Read every PID currently in `D/cgroup.procs` and write each to `D/system/cgroup.procs`.
   - *Fast Sandbox microVM synergy:* On Fast Sandbox, the guest `/init` PID 1 (`SANDBOX_HEARTBEAT` emitter) resides in the root cgroup alongside `execd`. Migrating all initial `D/cgroup.procs` into `D/system` both satisfies the Cgroup v2 *no internal processes* rule on `D` and protects the guest heartbeat loop inside `system/` from workload CPU starvation and workload OOM kills.
3. **Enable controllers selectively:** Read `D/cgroup.controllers` and write available controllers from `{+cpu, +memory, +pids}` to `D/cgroup.subtree_control`. Each controller degrades independently if absent from `cgroup.controllers`.
4. **Configure `system/` and `workload/`:**
   - `cpu`: write `system_cpu_weight` to `D/system/cpu.weight` and `workload_cpu_weight` to `D/workload/cpu.weight`. If `workload_cpu_max_pct > 0`, compute quota from the sandbox's `D/cpu.max` (or `CPUCount() * 100000` when `D/cpu.max` is `max`) and write `<quota> 100000` to `D/workload/cpu.max`.
   - `memory`: if `workload_memory_max` is set, resolve bytes (literal or `%` of `D/memory.max` / guest total) and write to `D/workload/memory.max`. OOM kills triggered by workload memory spikes stay strictly inside `D/workload` and never scan `execd` or `high`-tier platform daemons in `D/system`.
   - `pids`: if `workload_pids_max > 0`, write to `D/workload/pids.max`, guaranteeing PID headroom in `D/system` for emergency `qos: "high"` WebShells even during a workload fork bomb.
5. **Race-free child placement:** Hold `O_PATH | O_DIRECTORY | O_CLOEXEC` file descriptors for `D/system` and `D/workload`. In `launchManagedWith`, attach `SysProcAttr.UseCgroupFD = true` and `CgroupFD = targetFD` (`CLONE_INTO_CGROUP`, Linux >= 5.7). If `clone3` with `CLONE_INTO_CGROUP` returns `ENOSYS`/`EPERM` (e.g., `clone3compat` active or older kernel), retry without `UseCgroupFD` and write the child PID to `<target>/cgroup.procs` under `initReaper.mu` immediately after `Start()`.

### 5. Orthogonality with OSEP-0018 Privilege Hardening

`[qos]` and `[hardening]` compose via independent flags in the shared `opensandbox-launcher` policy:

| `[hardening]` | `[qos]` | Process `qos` | Launcher invoked? | Flags applied in child |
|---|---|---|---|---|
| off | off | any | No | Direct `exec` (today's default) |
| off | on | `"high"` | No | Direct `exec` in `high` / `system` cgroup (`nice = 0`, baseline `oom_score_adj`) |
| off | on | `"standard"` | **Yes** | `FLAG_QOS_LOW` (`nice` + `oom_score_adj`) + credential env strip only (no cap-drop, no `no_new_privs`, no seccomp/Landlock) |
| on | off | any | **Yes** | `FLAG_CAP_DROP` (+ optional UID/Landlock/seccomp) at `nice = 0` and baseline `oom_score_adj` |
| on | on | `"high"` | **Yes** | `FLAG_CAP_DROP` (+ optional UID/Landlock/seccomp) in `system` cgroup at `nice = 0` and baseline `oom_score_adj` |
| on | on | `"standard"` | **Yes** | `FLAG_CAP_DROP` + `FLAG_QOS_LOW` (+ optional UID/Landlock/seccomp) in `workload` cgroup |

Internal helper processes exempted by `withoutHardening()` (`filesystem_worker` and the outer `bwrap` binary for isolated sessions):
- `filesystem_worker` serves single-shot file API requests on behalf of `execd` and runs in `high`.
- Isolated sessions (`bwrap`): when `[qos]` is enabled, the outer `bwrap` launch receives `workload` cgroup placement and L1 scheduler/OOM floor (via a lightweight `FLAG_QOS_LOW`-only wrap or pre-exec drop inside `opensandbox-session-gate`), so isolated session workloads also respect the `standard` tier without breaking `bwrap`'s namespace setup.

### 6. Non-Blocking Saturation and I/O-Stall Diagnostics (`GET /metrics`)

#### Eliminating the 1-second blocking sleep

`MetricController.readMetrics()` (`pkg/web/controller/metric.go`) currently calls `resources.CPUPercent(time.Second)`, blocking every `GET /metrics` HTTP request for 1000ms. Meanwhile, `pkg/resources` already reads cumulative cgroup/host CPU counters.

Replace the per-request sleep with a lightweight background 1-second rolling sampler in `pkg/resources`:
- A single ticker samples cumulative CPU usage every 1s and caches the latest `cpu_used_pct` atomically.
- Both `GET /metrics`, `GET /metrics/watch`, and OpenTelemetry (`telemetry/system.go`) read the cached sample in `<1ms` without resetting each other's measurement window.

#### Additive pressure and task-state fields on `Metrics`

Extend `Metrics` (`specs/execd-api.yaml` and `pkg/web/model/metric.go`) with optional fields (`omitempty` when unavailable on non-Linux or older kernels; existing 5 required fields remain unchanged):

| Field | Type | Source (in-memory pseudo-FS only) | Diagnostic meaning |
|---|---|---|---|
| `cpu_nr_throttled` | `int64` | cgroup v2/v1 `cpu.stat` (`nr_throttled`) | Cumulative count of CFS bandwidth-throttled periods |
| `cpu_throttled_usec` | `int64` | v2 `cpu.stat` `throttled_usec` (v1 `throttled_time / 1000`) | Cumulative microseconds tasks were throttled by CPU limit |
| `cpu_some_pressure_avg10` | `float64` | cgroup `cpu.pressure` (`some avg10`), fallback `/proc/pressure/cpu` | % of wall time (10s window) at least one runnable task was delayed waiting for CPU |
| `io_full_pressure_avg10` | `float64` | cgroup `io.pressure` (`full avg10`), fallback `/proc/pressure/io` | % of wall time (10s window) all non-idle tasks were stalled on block/FS I/O |
| `mem_full_pressure_avg10` | `float64` | cgroup `memory.pressure` (`full avg10`), fallback `/proc/pressure/memory` | % of wall time (10s window) tasks were stalled in memory reclaim/thrashing |
| `procs_running` | `int` | `/proc/[pid]/stat` scan (`state == 'R'`) | Runnable threads competing for CPU inside the sandbox PID namespace |
| `procs_uninterruptible` | `int` | `/proc/[pid]/stat` scan (`state == 'D'`) | Tasks wedged in uninterruptible kernel I/O sleep (`TASK_UNINTERRUPTIBLE`) |

**Strict no-disk-I/O rule:** Reading `Metrics` must only read `/sys/fs/cgroup/*` and `/proc/[pid]/stat` (never `/proc/[pid]/cmdline` if it requires faulting user pages on swap, and never `stat()` on rootfs or mounted volumes). This guarantees `GET /metrics` responds in milliseconds even when `/workspace` is completely wedged.

#### Operator triage matrix

With `execd` protected in the `high` tier and `GET /metrics` non-blocking, platforms classify "unresponsive sandbox" incidents deterministically:

| Probe result | Root cause | Standard remediation |
|---|---|---|
| `GET /ping` times out or connection refused | **Host / VM / Runtime failure** (node down, network partition, VMM wedged) | Inspect `GET /sandboxes/{id}/diagnostics/events`; recycle or recreate sandbox |
| `GET /ping` OK; `cpu_used_pct` near 100%, `cpu_throttled_usec` / `cpu_some_pressure_avg10` high, `procs_uninterruptible == 0` | **In-sandbox CPU saturation** (runaway user workload) | Connect via `POST /pty` (`{"qos": "high"}`) or run `POST /command` (`{"qos": "high"}`) to inspect (`ps`/`top`) and `kill` offending processes |
| `GET /ping` OK; `procs_uninterruptible > 0` and/or `io_full_pressure_avg10` high | **Storage / I/O hang** (wedged volume, NAS, or FUSE mount) | Connect via `POST /pty` (`{"qos": "high", "cwd": "/", "command": "/bin/sh --noprofile --norc"}`), inspect `/proc/<pid>/wchan`, recover backing mount or recreate sandbox |

### 7. Capability Probing and Degradation Reporting

Extend `CapabilitiesResponse` (`GET /v1/isolated/capabilities` in `specs/execd-api.yaml`) with an optional `qos` sibling object alongside `hardening`, reusing `HardeningLayerState` (`active` | `disabled` | `degraded` | `unsupported` + `message`):

```json
{
  "hardening": { "...": "..." },
  "qos": {
    "enabled": true,
    "entrypoint_qos": "standard",
    "scheduler": {
      "state": "active"
    },
    "cgroup": {
      "state": "degraded",
      "message": "cgroup v2 root /sys/fs/cgroup is read-only; falling back to scheduler QoS only"
    }
  }
}
```

If `[qos] enabled = true` is paired with `init_mode == "none"` (`EXECD_INIT` unset) and `entrypoint == "standard"`, `scheduler` reports `degraded` with a clear message noting that `execd`-spawned processes are covered while the bootstrap-launched `ENTRYPOINT` is not wrapped (mirroring OSEP-0018's drift reporting).

### 8. Relationship to Existing Proposals

- **[OSEP-0018 (execd as Sandbox Init)](0018-execd-as-sandbox-init.md):** Provides the PID 1 / subreaper topology, `launchManaged` process registry, `opensandbox-launcher` binary, and isolation TOML delivery channel that this proposal builds upon directly.
- **[OSEP-0010 (OpenTelemetry Instrumentation)](0010-opentelemetry-instrumentation.md):** The shared non-blocking sampler in `pkg/resources` serves both `GET /metrics` and OTel gauges without contention; `telemetry`'s existing cgroup OOM kill watcher complements L1/L2 OOM protection by recording workload OOM events.
- **[OSEP-0020 (Sandbox Lifecycle Hooks)](0020-sandbox-lifecycle-hooks.md):** Lifecycle `preStart` and `periodic` hooks gain optional `qos` selection so platform daemons and health hooks are neither CPU-starved nor OOM-selected ahead of user workloads.

## Test Plan

- **Unit tests (`components/execd`):**
  - `[qos]` and `[qos.cgroup]` TOML parsing, validation errors (out-of-range `nice` or `oom_score_adj`, invalid `qos` enum, `[qos.cgroup]` without `[qos]`), and `EXECD_QOS_ENABLED` / `EXECD_QOS_ENTRYPOINT` overrides + env-strip verification.
  - `launcher` policy serialization across all 6 `[hardening]` x `[qos]` combinations (§5 table).
  - Cgroup v2 setup logic against synthetic cgroupfs trees: PID migration into `system/`, selective controller enablement, `cpu.max` percentage calculation, `memory.max` percentage/byte parsing, and clean `degraded` fallback on read-only mounts or `EBUSY`.
  - Non-blocking `Metrics` parser tests for v1/v2 `cpu.stat` throttle counters, PSI files, and `/proc/[pid]/stat` `R`/`D` state counting.
  - HTTP controller tests for `qos` validation on `POST /pty`, `POST /command`, and `POST /session` (including no-op acceptance when `[qos]` is off, and `400` on invalid values).
- **Linux integration tests (`components/execd`):**
  - Launch `standard` child (including cross-UID launch without `CAP_SYS_NICE` or `CAP_SYS_RESOURCE`) and verify `/proc/<pid>/stat` reports `nice = 19` (or `SCHED_IDLE`), `RLIMIT_NICE = 0`, `/proc/<pid>/oom_score_adj == 500`, and both `renice -n 0` and writing `0` to `/proc/self/oom_score_adj` inside the child fail with `EACCES`/`EPERM` (plus verify `current = 800` is preserved when `oom_score_adj = 500`).
  - Launch `qos: "high"` `/command` and `/pty` alongside `[hardening] enabled = true` + `[qos] enabled = true` and verify `nice = 0` and baseline `oom_score_adj` **while** `CapEff = 0`, `NoNewPrivs = 1`, and `Seccomp = 2` remain enforced.
- **End-to-end tests (`tests/python` & `scripts/fast-sandbox-env`):**
  - **CPU starvation benchmark E2E (Docker & Kubernetes):** Constrain a sandbox to `1.0` CPU (`--cpus=1`), enable `EXECD_QOS_ENABLED=true`, start 16 CPU-bound `standard` spinner processes (`cpu_used_pct ~ 100%`, `cpu_nr_throttled > 0`), and assert:
    1. `GET /ping` and `GET /metrics` respond in `<100ms` and report throttling + `procs_running >= 16`.
    2. Opening a `POST /pty` WebSocket session with `{"qos": "high"}` and running `echo ready && kill ...` completes within `<1s` (whereas a baseline test without QoS exhibits severe latency).
  - **OOM victim steering E2E (Docker & Kubernetes `Guaranteed`):** Constrain a sandbox to `512Mi` memory with `EXECD_QOS_ENABLED=true` and `EXECD_QOS_ENTRYPOINT=high`, run a resident `high`-tier daemon holding ~150 MiB RSS, spawn multiple `standard` workers allocating ~80 MiB each until cgroup OOM triggers, and assert that `execd` and the `high`-tier daemon survive intact while a `standard` worker is OOM-killed.
  - **Fast Sandbox L2 Cgroup E2E:** Enable `[qos]` + `[qos.cgroup]` in a Fast Sandbox template, verify `qos.cgroup.state == "active"`, confirm guest `/init` and `execd` reside in `system/`, and verify both a workload memory bomb (`workload/memory.max`) and fork storm (`workload/pids.max`) stay confined to `workload/` while `qos: "high"` `/pty` spawns `/bin/sh` and cleans up.

## Drawbacks

1. **Same-token trust model for `qos: "high"`:** Any caller holding `X-EXECD-ACCESS-TOKEN` can pass `"qos": "high"`. This is intentional (matching creator-opted posture without multi-token complexity): an untrusted caller who already holds the sandbox's execution token can run arbitrary commands anyway, while well-behaved SDKs omit `qos` (defaulting to `"standard"`) and platform consoles reserve `"qos": "high"` for operator WebShells and control daemons.
2. **L1 vs. L2 memory protection boundary:** In unprivileged OCI containers where `/sys/fs/cgroup` is read-only, L1 `oom_score_adj` steers the kernel OOM killer to sacrifice `standard` workloads before `high`-tier daemons (effective in Docker and Kubernetes `Guaranteed` pods, bounded by kubelet's high initial `oom_score_adj` in `Burstable` pods), whereas capping aggregate workload memory *before* the container hits OOM requires L2 `workload/memory.max`.
3. **`POLICY_VERSION` bump between `execd` and `opensandbox-launcher`:** Requires both binaries to match, which is already guaranteed because the platform stages `execd` and `opensandbox-launcher` together from the same image.

## Alternatives

1. **Hardcode `ENTRYPOINT` and `/pty` as `high` priority automatically.** Rejected: in standard user sandboxes `ENTRYPOINT` runs user workloads, and AI agents frequently execute heavy builds inside `/pty` terminals. Hardcoding roles would re-introduce CPU starvation and OOM ties whenever user code runs in `ENTRYPOINT` or `/pty`.
2. **Add dedicated `/v1/processes` list and kill endpoints to `execd`.** Rejected for initial scope (simplicity first): once `qos: "high"` guarantees `/pty` and `/command` responsiveness (and L2 `workload/pids.max` reserves PID headroom where cgroups are writable), standard POSIX tools (`ps`, `top`, `kill`) over existing endpoints cover interactive and automated remediation without expanding `execd`'s API surface. Dedicated endpoints can be reconsidered later if single-UID container `pids_limit` exhaustion without L2 cgroups proves common.
3. **Separate platform-admin token for `qos: "high"`.** Rejected: adds token distribution, rotation, and RuntimeBinding complexity across Server, K8s Pool, Fast Sandbox, and SDKs, whereas `qos: "high"` grants no security privilege escalation (§5).
4. **Rely on Kubernetes `kubectl exec` for emergency WebShell.** Rejected: unavailable on Fast Sandbox microVMs and standalone Docker runtimes, and incompatible with Cgroup v2 root controller delegation (`EBUSY`).

## Infrastructure Needed

None. Existing Docker, Kubernetes, and Fast Sandbox E2E lanes in GitHub Actions already exercise CPU/memory-limited sandboxes, `execd` init mode, and `opensandbox-launcher`.

## Upgrade & Migration Strategy

- **100% backward compatible by default:** `[qos]` defaults to `false`; `GET /metrics` keeps all existing required fields and types while adding optional fields; `qos` on `/pty`, `/command`, and `/session` is optional and defaults to `"standard"`.
- **Smooth WebShell unification:** Console and internal platform WebShell gateways can immediately migrate from `kubectl exec` to `execd` `POST /pty` with `{"qos": "high"}` (against updated `execd` images) even before individual sandbox images turn on `[qos]`, because `"qos": "high"` is accepted as a no-op when `[qos]` is disabled.
- **SDK alignment:** Per the repository's spec-alignment rule, `specs/execd-api.yaml` (`/pty` routes, `ProcessQoS`, `Metrics` pressure fields, `CapabilitiesResponse.qos`) and generated/handwritten Sandbox SDK models will be updated together in the implementing PRs.
