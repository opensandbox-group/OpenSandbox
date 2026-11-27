---
title: In-Sandbox Process QoS and Saturation Diagnostics
authors:
  - "@hittyt"
  - "@Pangjiping"
creation-date: 2026-11-17
last-updated: 2026-11-26
status: implementing
---

# OSEP-0031: In-Sandbox Process QoS and Saturation Diagnostics

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Static configuration](#static-configuration)
  - [Launcher behavior](#launcher-behavior)
  - [Cgroup v2 isolation (Phase 2)](#cgroup-v2-isolation-phase-2)
  - [Saturation diagnostics](#saturation-diagnostics)
  - [Capability reporting](#capability-reporting)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Future Work](#future-work)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Building on [OSEP-0018](0018-execd-as-sandbox-init.md), this proposal lets a sandbox creator protect `execd` and an explicitly selected, `execd`-managed image ENTRYPOINT from resource starvation caused by ordinary workloads. Phase 1 is opt-in, best effort, and needs neither extra Linux capabilities nor a writable cgroup: it applies per-process CPU priority (`nice`/`SCHED_IDLE`, `RLIMIT_NICE=0`) and monotonic `oom_score_adj` steering in `opensandbox-launcher`. Phase 2 adds opt-in cgroup v2 group isolation (`[qos.cgroup]`) for delegated cgroup v2 environments and dedicated microVM guests (such as Firecracker under `fast-sandbox`), optionally auto-mounting `cgroup2` at `/sys/fs/cgroup` and splitting `opensandbox-system` from `opensandbox-workload` for group CPU weights/limits, memory protection/headroom, and PID headroom while failing open when unavailable. Without `execd` init mode, the bootstrap launches the ENTRYPOINT outside this policy. Non-blocking sampling on `GET /metrics` helps operators distinguish resource pressure from other causes of a slow response.

## Motivation

1. **CPU contention:** A parallel build can put dozens of runnable threads in competition with `execd`'s HTTP and WebSocket handlers. Lowering the workers' scheduling weight gives the handlers a better chance to run while CPU time is available. It does not make a shell spawned by those handlers a priority shell.
2. **Collateral OOM kills:** Linux scores individual candidates, not the combined footprint of all workers in an application. With equal `oom_score_adj`, a resident platform daemon can score higher than any one worker even when the workers collectively exhaust memory. For a memory-cgroup OOM the adjustment is scaled by the memcg limit; for an unconstrained global OOM it is scaled against system-wide pages. A shared negative adjustment (such as `-997`) is an offset, not a clamp to one: points can be negative, and an equal-scoring eligible candidate can replace the previous candidate. Neither case implies that the earliest-started process dies. See [Linux v6.6 victim scoring and selection](https://github.com/torvalds/linux/blob/v6.6/mm/oom_kill.c#L201-L350).
3. **Slow diagnostics:** `MetricController.readMetrics` calls `resources.CPUPercent(time.Second)` in the request path. A slow probe alone cannot distinguish the cost of sampling, cgroup throttling, memory reclaim, an I/O stall, or loss of connectivity.
4. **Aggregate resource exhaustion:** Per-task `nice` and `oom_score_adj` alone cannot prevent a multi-threaded build from diluting `execd`'s CPU share, prevent memory reclaim stalls before an OOM kill, or stop a fork bomb from exhausting PIDs. In dedicated microVM guests (such as Firecracker under `fast-sandbox`), the guest kernel is owned by the sandbox, yet `/sys/fs/cgroup` is often unmounted by default, leaving cgroup v2 group isolation unused.

### Goals

- Improve `execd`'s CPU share relative to managed workloads and steer OOM selection in Phase 1 without requiring extra capabilities or a writable cgroup.
- Provide opt-in Phase 2 cgroup v2 group isolation (`opensandbox-system` vs `opensandbox-workload`) in microVM guests (including Firecracker auto-mount of `/sys/fs/cgroup`) and delegated cgroup v2 containers to prevent thread-storm CPU dilution, memory reclaim/OOM starvation, and fork-bomb PID exhaustion.
- Let a creator retain baseline treatment for a trusted ENTRYPOINT, without guessing its role or exposing a priority choice to execution clients.
- Remove the per-request CPU sampling delay and expose available pressure signals without reading workload volumes.

### Non-Goals

- No hard isolation in unprivileged containers that lack a writable cgroup v2 hierarchy; Phase 2 fails open cleanly when cgroup v2 is unavailable or read-only.
- No external server switch, no changes to launch requests, lifecycle hook schemas, or SDK launch methods; no dedicated emergency shell or process-management endpoint.
- No Windows scheduling policy or disk bandwidth throttling.

## Requirements

| ID | Requirement | Priority |
|---|---|---|
| R1 | With QoS disabled (the default), scheduling, OOM adjustments, resource limits and cgroup placement remain unchanged. | Must |
| R2 | With QoS enabled, managed workloads start as `standard`. Only a managed ENTRYPOINT may be configured as `high`; lifecycle hooks, commands, sessions, PTYs and isolated workloads always start as `standard`. | Must |
| R3 | In Phase 1, apply CPU settings and an initial OOM adjustment to the child before workload execution, including across UID changes, without requiring extra capabilities. Never decrease an inherited OOM adjustment or rely on the adjustment being locked. | Must |
| R4 | QoS does not enable, weaken or bypass OSEP-0018 hardening. | Must |
| R5 | Metrics readers share a CPU sample without sleeping in the request path. Pressure and process-state fields are optional; existing required fields retain their names and types. | Must |
| R6 | Report startup QoS support and incomplete coverage separately from observed runtime pressure. | Must |
| R7 | With Phase 2 cgroup v2 isolation enabled, `execd` optionally auto-mounts `cgroup2` at `/sys/fs/cgroup` when unmounted, creates sibling `opensandbox-system` and `opensandbox-workload` leaf cgroups obeying cgroup v2's no-internal-process rule, places processes according to R2 (with internal filesystem workers staying in `opensandbox-system`), degrades independently per controller (`cpu`, `memory`, `pids`), and fails open when cgroup v2 is unavailable or read-only. | Must |
| R8 | Moving `execd` into `opensandbox-system` must not narrow sandbox observability: `pkg/resources` and `pkg/telemetry` OOM watching remain anchored to the sandbox base cgroup `<base>`, and `pkg/ebpf` matches the full sandbox subtree under `<base>` (including `opensandbox-system` and `opensandbox-workload`) rather than only `<base>`'s exact cgroup ID. | Must |

## Proposal

| Launch path | Phase 1 treatment (`[qos]`) | Phase 2 cgroup placement (`[qos.cgroup]`) |
|---|---|---|
| `execd` and internal filesystem API workers | Retain inherited scheduler and OOM settings (`high` is not a priority boost). | Placed in `opensandbox-system` (higher `cpu.weight`, `memory.low` protection). |
| Image ENTRYPOINT launched by `execd` | `standard` unless the creator explicitly selects `high`. Descendants inherit the starting settings. Use `high` only for a trusted daemon, not for a supervisor that also starts user builds. | `opensandbox-system` when `entrypoint = "high"`; otherwise `opensandbox-workload`. |
| Lifecycle hooks, commands, sessions, PTYs and isolated workloads | `standard`, with no per-request or per-hook override. A platform daemon that needs baseline treatment must run as the configured ENTRYPOINT, not as a hook. | Always `opensandbox-workload` (lower `cpu.weight`, optional `cpu.max`, `memory.max`/`memory.high`, `pids.max`). |
| ENTRYPOINT launched by the external bootstrap | Unchanged and outside coverage. Enable the OSEP-0018 init path (`EXECD_INIT`) to manage it. | Outside coverage; reported in capability diagnostics. |

### Notes/Constraints/Caveats

In Phase 1, the two tiers share one cgroup. CPU weights matter when runnable tasks compete for available CPU; after the sandbox exhausts its CPU quota, the entire group, including `execd`, waits for replenishment. Memory reclaim can stall `execd` before an OOM even occurs, and there is no reserved memory or PID headroom.

In Phase 2, `opensandbox-system` and `opensandbox-workload` run in sibling cgroup v2 leaf groups when a writable unified hierarchy is available (such as a Firecracker microVM guest or a delegated cgroup v2 container). Group `cpu.weight` prevents thread-count dilution across all workload threads, `memory.low` and `memory.max` reserve memory headroom for `execd`, and `pids.max` caps workload process expansion. However, if the sandbox hits an external host-side CPU quota (such as a container or host Firecracker cgroup limit), the entire sandbox still waits for replenishment, and if cgroup v2 is read-only or lacks a controller, Phase 2 degrades cleanly rather than blocking startup.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Many runnable workers dilute per-task nice weights | Phase 1 offers `SCHED_IDLE` as an alternative (`1024/(1024+15N)` on one CPU for nice-0 vs N nice-19 threads). Phase 2 group `cpu.weight` (and optional `cpu.max`) bounds the aggregate workload cgroup regardless of thread count. |
| A workload changes its OOM adjustment, triggers reclaim stalls, or exhausts memory/PIDs | Phase 1 sets an initial `oom_score_adj` preference without locking `oom_score_adj_min`. Phase 2 adds `system/memory.low`, `workload/memory.max` (and optional `memory.high`), and `workload/pids.max` so memory and PID pressure stay contained inside `opensandbox-workload`. |
| A UID 0 or `CAP_SYS_ADMIN` workload rewrites cgroup limits or tampers with `opensandbox-system` (e.g., `cgroup.freeze`, `memory.max`, `pids.max`) | Phase 2 alone protects against accidental resource exhaustion, not an adversarial root workload sharing a writable cgroup mount. Protecting against untrusted workloads requires: (1) running workloads as a non-root UID/GID, (2) keeping the writable cgroup hierarchy private to `execd` (or exposing `/sys/fs/cgroup` read-only / denying access via OSEP-0018 Landlock), and (3) dropping `CAP_SYS_ADMIN` so workloads cannot remount `cgroup2` read-write or mount a fresh instance. |
| Runtime lacks a scheduling, cgroup v2, or procfs facility | Report the limitation and continue the workload (fail-open per layer and per controller); do not assume Docker, Kubernetes, gVisor, Kata and microVM guests implement identical semantics. |
| A timeout is mistaken for a dead host | Compare samples with external runtime events. Missing pressure data is unknown, not zero. |

## Design Details

### Static configuration

Use the existing creator-provided isolation TOML selected by `EXECD_ISOLATION_CONFIG`. It is the only QoS configuration surface. Platforms using stock images must mount or bake this file to opt in.

```toml
[qos]
enabled = true
entrypoint = "standard"
nice = 19
scheduler = "other"
oom_score_adj = 500

[qos.cgroup]
enabled = true
auto_mount = true
system_cpu_weight = 1000
workload_cpu_weight = 100
workload_cpu_max_percent = 0
system_memory_low_bytes = 67108864
memory_reserve_bytes = 134217728
workload_memory_max_bytes = 0
workload_memory_high_bytes = 0
pids_reserve = 128
workload_pids_max = 0
```

Omitting `[qos]` or setting `enabled = false` leaves Phase 1 process QoS off; omitting `[qos.cgroup]` or setting `[qos.cgroup].enabled = false` leaves Phase 2 cgroup v2 isolation off. Phase 1 and Phase 2 may be enabled independently or together, and both share `[qos].entrypoint` (default `"standard"`). All other values shown in the TOML block above are defaults when their enclosing section is enabled (including `auto_mount = true`, which is a no-op when `cgroup2` is already mounted). Validate `entrypoint` as `standard` or `high`, `scheduler` as `other` or `idle`, `nice` in `1..19` (used with `other`), and `oom_score_adj` in `0..1000` (an explicit zero disables adjustment). For `[qos.cgroup]`, validate `system_cpu_weight` and `workload_cpu_weight` in `1..10000`, `workload_cpu_max_percent` in `0..99` (`0` disables `cpu.max`), non-negative memory and PID fields, and `workload_memory_high_bytes <= workload_memory_max_bytes` when both are non-zero. Reject invalid static configuration at startup rather than silently changing the creator's choice.

### Launcher behavior

`launchManagedWith` selects the tier from the launch path, not the request payload. Add an independent QoS flag and its settings to the versioned policy shared by `execd` and `opensandbox-launcher`. Invoke the launcher for a `standard` workload even when hardening is off. A `high` ENTRYPOINT skips only the QoS adjustment. Preserve the existing hardening policy, credential handling and launch failure behavior in both tiers. Internal file API helpers act on behalf of `execd`, not as workload entry points; apply the standard policy to isolated workloads without changing the namespace helper's hardening exemption.

1. The launcher adjusts itself before executing the workload rather than racing a parent-side update against `execve`. Set both `RLIMIT_NICE` limits to zero and apply the selected scheduling policy: nice 19 by default, or `SCHED_IDLE`. This restricts an unprivileged child's ability to increase priority; no extra capability is requested.
2. For OOM steering, open `/proc/self/oom_score_adj` before an in-launcher UID switch so that a later procfs ownership change does not prevent opening it. After the requested identity/capability changes and before Landlock/seccomp, raise the adjustment only if it is below the configured nonzero target. Close the descriptor before executing the workload.
3. An unprivileged write changes the current adjustment, **not** `oom_score_adj_min`. Linux only updates that minimum on a privileged write with `CAP_SYS_RESOURCE`; the child can subsequently lower the adjustment as far as the existing minimum permits. Phase 1 neither requests that capability nor promises a lock. See [Linux v6.6 procfs write semantics](https://github.com/torvalds/linux/blob/v6.6/fs/proc/base.c#L1084-L1112).
4. If a QoS step is unavailable, report the failed setting and continue with the settings that succeeded. This fail-open behavior applies only to QoS, not to identity changes or other existing hardening checks.

### Cgroup v2 isolation (Phase 2)

When `[qos.cgroup].enabled = true`, `execd` initializes a two-leaf cgroup v2 hierarchy at startup before launching any workload, degrading cleanly if cgroup v2 is unavailable or read-only:

1. **Optional `cgroup2` auto-mount (dedicated guests such as Firecracker):** Check `/proc/self/mounts` for an existing `cgroup2` mount. If absent and `auto_mount = true`, verify `/proc/filesystems` advertises `cgroup2`, then run `mkdir -p /sys/fs/cgroup` and `mount("none", "/sys/fs/cgroup", "cgroup2", MS_NOSUID|MS_NODEV|MS_NOEXEC, "")`. Failure is logged and degrades the layer to `unsupported`/`degraded` without blocking startup.
2. **Threat model and cgroupfs security boundary:** Mounting the guest's own `cgroup2` does not expand the VM-to-host escape surface, but exposing a writable `/sys/fs/cgroup` to workloads running as UID 0 in the same mount namespace creates an in-sandbox threat against `execd` itself — a root workload can not only raise or escape `opensandbox-workload` limits, but also write to `opensandbox-system` (for example, `cgroup.freeze` or lowering `memory.max`/`pids.max`) to freeze or kill `execd`. Therefore, `[qos.cgroup]` by itself defends against accidental resource exhaustion; defending against adversarial or untrusted workloads requires combining it with: (a) running workloads under a non-root UID/GID so they cannot write root-owned cgroup control files, (b) keeping the writable cgroup hierarchy private to `execd` (or exposing `/sys/fs/cgroup` read-only / denying access via OSEP-0018 Landlock), and (c) dropping `CAP_SYS_ADMIN` for workloads (via OSEP-0018 `[hardening]`) so they cannot remount `/sys/fs/cgroup` read-write or mount a fresh `cgroup2` elsewhere.
3. **Base cgroup discovery and tree creation (`no internal process` safe):** Resolve `execd`'s current unified-hierarchy directory `<base>` via `/proc/self/cgroup` (`0::`) and `/proc/self/mountinfo` (rejecting hybrid v1+v2 setups where memory accounting lives on v1). Record `<base>` as the sandbox cgroup scope, then initialize in strict order so both root (`/sys/fs/cgroup`) and delegated non-root cgroups work identically:
   - Create `<base>/opensandbox-system` and move `execd`'s own PID into `<base>/opensandbox-system/cgroup.procs`.
   - Read `<base>/cgroup.controllers` and independently enable `+cpu`, `+memory`, and `+pids` in `<base>/cgroup.subtree_control`; if any individual controller is absent or rejected, log a warning, mark `degraded`, and continue with the remaining controllers.
   - Create `<base>/opensandbox-workload` and write the configured controller limits.
4. **Controller budget derivation:**
   - **CPU:** Write `system_cpu_weight` to `opensandbox-system/cpu.weight` and `workload_cpu_weight` to `opensandbox-workload/cpu.weight`. Because cgroup weights apply per group rather than per thread, `opensandbox-system` retains its configured share even if `opensandbox-workload` spawns hundreds of runnable threads, while idle system capacity remains fully usable by workloads. When `workload_cpu_max_percent` is `1..99`, write `<quota> 100000` to `opensandbox-workload/cpu.max` using the effective available core count (`<base>/cpu.max` if limited, otherwise `runtime.NumCPU()`).
   - **Memory:** Write `system_memory_low_bytes` (when non-zero) to `opensandbox-system/memory.low` to protect `execd` pages from reclaim. Determine effective total memory from `<base>/memory.max` (if finite) or `/proc/meminfo` `MemTotal`. Write `opensandbox-workload/memory.max` from `workload_memory_max_bytes` if non-zero, otherwise `total - memory_reserve_bytes` when `memory_reserve_bytes > 0` and `total > memory_reserve_bytes` (skip with a warning if the reserve would exceed total memory). Write `workload_memory_high_bytes` when non-zero.
   - **PIDs:** Write `opensandbox-workload/pids.max` from `workload_pids_max` if non-zero, otherwise `<base>/pids.max - pids_reserve` when `<base>/pids.max` is finite and exceeds `pids_reserve`. When `<base>/pids.max` is `max` (such as a Firecracker guest root) and `workload_pids_max == 0`, skip setting a PID limit and record a diagnostic note rather than guessing an arbitrary cap.
5. **Atomic process placement and pre-exec `clone3compat` fallback:** Keep an `O_PATH|O_DIRECTORY|O_CLOEXEC` file descriptor open to `<base>/opensandbox-workload`. `execd`, internal filesystem API workers, and a managed ENTRYPOINT with `entrypoint = "high"` stay in `opensandbox-system`. All `standard` launches (managed `standard` ENTRYPOINT, commands, bash sessions, PTYs including pipe fallback, lifecycle hooks, and isolated workloads) attach `SysProcAttr{UseCgroupFD: true, CgroupFD: workloadFD}` (Linux 5.7+ `clone3(CLONE_INTO_CGROUP)`) without overwriting existing `SysProcAttr` fields (`Setpgid`, `Credential`, PTY `Setsid`/`Setctty`). When `clone3compat` is active (returning `ENOSYS` for `clone3`) or the kernel rejects `CLONE_INTO_CGROUP` with `ENOSYS`/`EPERM`, do not migrate the child asynchronously from the parent after `exec.Cmd.Start` (which races with child `fork()` and can leave descendants in `opensandbox-system` outside `pids.max`); instead, complete placement atomically before workload `execve` — either via a trusted child-side pre-exec step (`opensandbox-launcher` writing its PID to a pre-opened `opensandbox-workload/cgroup.procs` descriptor before dropping identity/capabilities/Landlock/seccomp and closing the descriptor before `execve`) or by holding the child behind a pre-exec stop/ack pipe barrier until the parent's `cgroup.procs` write succeeds — caching the fallback mode so subsequent launches do not retry `CLONE_INTO_CGROUP`.
6. **Observability scope preservation:** Register `<base>` in `pkg/cgroupfs` so `pkg/resources` (`resources_linux.go`, `disk_linux.go`) and `pkg/telemetry/oom_linux.go` stay anchored to `<base>` rather than narrowing to `opensandbox-system`. In a Firecracker guest where `<base>` is the unlimited root `/`, `pkg/resources` continues falling back to `hostBackend`; in a bounded container, it continues reporting full-sandbox usage. Because cgroup v2 `memory.events` aggregates child cgroups recursively, anchoring `oom_linux.go` to `<base>` captures kills inside `opensandbox-workload` and attributes them via the existing first-level child cgroup scan. For `pkg/ebpf`, note that `audit.bpf.c` currently checks exact equality on `bpf_get_current_cgroup_id()`, so matching only `<base>`'s ID would silently drop events from child cgroups (`opensandbox-system` and `opensandbox-workload`); Phase 2 must extend the BPF filter to match the sandbox subtree rooted at `<base>` (for example, via `bpf_get_current_ancestor_cgroup_id()` at `<base>`'s hierarchy depth, or matching the configured set of leaf cgroup IDs).

### Saturation diagnostics

Replace the per-request one-second sleep with one background CPU sampler in `pkg/resources`. `GET /metrics`, `GET /metrics/watch` and OpenTelemetry read the latest sample without resetting one another's measurement window. Preserve the watch cadence. The handler does not wait for a fresh interval or promise an HTTP response deadline.

Extend `Metrics` with optional fields. Use only procfs and cgroupfs; do not probe a workload mount or read process command lines. A missing field means unavailable. In particular, PSI depends on kernel configuration, boot settings and runtime exposure. If pressure files cannot be read, omit their fields; compare successive `cpu.stat` counters when available. Do not substitute host-wide pressure for sandbox pressure on a shared host. In cgroup v2, `nr_throttled` and `throttled_usec` are recorded on the cgroup whose own `cpu.max` fired and are not rolled up into parent `cpu.stat`; therefore, when Phase 2 is active (under either a root cgroup `/` or a delegated non-root `<base>`), read both quota-bearing scopes (`<base>/cpu.stat` when present and `opensandbox-workload/cpu.stat` when `workload_cpu_max_percent > 0`) so neither external parent throttling nor internal workload throttling is missed. Under a root cgroup `/` (such as a Firecracker guest), read guest-wide `/proc/pressure/*` (or `opensandbox-workload/*.pressure`) as available.

| Field | Type | Source (in-memory pseudo-FS only) | Diagnostic meaning |
|---|---|---|---|
| `cpu_nr_throttled` | `int64` | cgroup v2/v1 `cpu.stat` (`nr_throttled`) | Cumulative count of CFS bandwidth-throttled periods |
| `cpu_throttled_usec` | `int64` | v2 `cpu.stat` `throttled_usec` (v1 `throttled_time / 1000`) | Cumulative microseconds tasks were throttled by CPU limit |
| `cpu_some_pressure_avg10` | `float64` | cgroup `cpu.pressure` (`some avg10`), guest-wide `/proc/pressure/cpu` only in a dedicated guest | % of wall time (10s window) at least one runnable task was delayed waiting for CPU |
| `io_full_pressure_avg10` | `float64` | cgroup `io.pressure` (`full avg10`), guest-wide `/proc/pressure/io` only in a dedicated guest | % of wall time (10s window) all non-idle tasks were stalled on block/FS I/O |
| `mem_full_pressure_avg10` | `float64` | cgroup `memory.pressure` (`full avg10`), guest-wide `/proc/pressure/memory` only in a dedicated guest | % of wall time (10s window) tasks were stalled in memory reclaim/thrashing |
| `procs_running` | `int` | `/proc/[pid]/stat` process-leader scan (`state == 'R'`) | Sampled runnable processes visible to execd (not a count of all runnable threads) |
| `procs_uninterruptible` | `int` | `/proc/[pid]/stat` process-leader scan (`state == 'D'`) | Sampled processes in uninterruptible sleep; not proof of a storage fault |

| Observation | Interpretation and next step |
|---|---|
| `/ping` times out | Inconclusive: check quota throttling, memory pressure, network and external runtime events before diagnosing host failure. |
| CPU saturation with increasing throttled time | The sandbox is hitting its quota. If needed, inspect processes through an ordinary PTY or command; like any other workload it may be slow. |
| Uninterruptible processes or elevated I/O pressure | Investigate storage and other kernel waits; start outside a suspect workload mount if a shell can be opened. |

### Capability reporting

Add an optional `qos` object to `GET /v1/isolated/capabilities` using the existing layer-state vocabulary (`active`, `disabled`, `degraded`, `unsupported`) and a diagnostic message. Report configured ENTRYPOINT treatment and startup support; this is not proof that every launch applied every setting. With init mode off, report incomplete ENTRYPOINT coverage while continuing to apply QoS to managed workloads. When Phase 2 (`[qos.cgroup]`) is configured, also report an optional `cgroup_qos` layer state with the active controllers (`cpu`, `memory`, `pids`), whether `cgroup2` was auto-mounted, and whether `cgroup.procs` fallback is active instead of `CLONE_INTO_CGROUP`. Pressure metrics remain useful independently of whether QoS is enabled.

## Test Plan

These checks are for the implementation, not results from this proposal:

- **Configuration and routing:** Cover defaults, explicit zero, invalid TOML values (both `[qos]` and `[qos.cgroup]`), a managed ENTRYPOINT in both tiers, and a bootstrap ENTRYPOINT outside coverage. Verify hooks, commands, sessions, PTYs and isolated workloads take the standard path regardless of ENTRYPOINT selection. With QoS off, compare launches to the existing behavior.
- **Linux launcher:** With and without hardening and across a UID change, inspect initial scheduler, nice limits and OOM adjustment. An inherited adjustment of 800 must stay 800 with a target of 500. In an unprivileged fixture with a known existing minimum, verify the initial raise does not install a new minimum. Check that a high ENTRYPOINT still receives all applicable hardening; simulate an unsupported QoS operation without changing hardening behavior.
- **Cgroup v2 isolation (Phase 2):** Unit-test configuration validation, hierarchy creation order, per-controller independent degradation, CPU/memory/PID budget derivation, `auto_mount` detection, dual-scope `cpu.stat` throttling reads (`<base>` + `opensandbox-workload`), and `cgroupfs` sandbox-root preservation (`resources` and `oom_linux` remaining anchored to `<base>`) against a temporary fake cgroupfs tree without requiring root. In a privileged Linux or Firecracker fixture, verify `cgroup2` auto-mount when unmounted, `/proc/<pid>/cgroup` placement across `execd`, filesystem workers, `high`/`standard` ENTRYPOINT, commands, sessions, PTYs, hooks, and isolated workloads, atomic pre-exec `cgroup.procs` fallback (no descendants left in `opensandbox-system`) when `clone3compat` is enabled, eBPF audit event capture from both `opensandbox-system` and `opensandbox-workload` when `[ebpf]` is also enabled, and non-root/hardened workload inability to modify `opensandbox-system` or `opensandbox-workload` control files. Under stress, verify `execd` `/ping` responsiveness against multi-threaded workload CPU burn, workload-contained OOM kills with `execd` survival and OOM watcher attribution, and `pids.max` containment against fork bombs.
- **CPU contention:** Pin a fixed pool of busy workers and `execd` to one CPU **without a bandwidth quota**. Compare repeated QoS-off/on trials with the same probe rate and compare p95 `/ping` and `/metrics` latencies; expect improvement under contention, not a fixed deadline. In a separate quota-limited run, check for increasing throttling counters and record probe latency without a pass/fail latency threshold. Measure short `/command` launch overhead separately with hardening off before publishing performance claims.
- **Memory pressure:** Check initial adjustments independently of victim selection. In a bounded memcg fixture with individual OOM kills and workers that retain the assigned adjustment, record victim selection over repeated QoS-off/on runs. Do not require that a specific process always survives or infer protection against reclaim stalls.
- **Diagnostics:** Test cached reads by concurrent metrics/watch/telemetry consumers, v1/v2 throttle units, absent PSI, sampled process states and a bootstrap ENTRYPOINT coverage warning. Verify missing optional data preserves the existing metrics response.

## Drawbacks

Every managed standard launch may incur an extra launcher exec and policy setup when hardening is off and Phase 1 is enabled. Hooks and interactive shells deliberately have no promotion path. Phase 1 per-process policy cannot bound aggregate worker CPU, memory or PID use; Phase 2 cgroup v2 isolation addresses those limits only in environments that provide a writable unified cgroup hierarchy (such as Firecracker microVMs or delegated containers).

## Alternatives

Running all ENTRYPOINTs or PTYs at baseline priority would also promote user builds run through those paths. The static ENTRYPOINT exception covers a creator-owned platform daemon without asking execution clients to classify workloads. Relying on guest `systemd` slices (as in some microVM distributions) would require shipping `systemd` as PID 1 in every template image; initializing `cgroup2` directly in `execd` works across minimal container-derived rootfs images without changing the guest init stack.

## Future Work

Revisit per-launch or per-hook QoS selection, block I/O (`io.weight`/`io.max`) throttling, and formalizing `/pty` in OpenAPI only if a concrete client needs them.

## Infrastructure Needed

No additional infrastructure for this proposal. Implementation measurements need Linux fixtures with controlled CPU placement, quota, memory limits, and (for Phase 2) a writable cgroup v2 or Firecracker microVM environment.

## Upgrade & Migration Strategy

Existing sandbox images keep their scheduling and cgroup behavior until the creator opts in via TOML (`[qos]` and/or `[qos.cgroup]`). For Firecracker microVMs where `/sys/fs/cgroup` is not pre-mounted by guest init, enable `[qos.cgroup]` with `auto_mount = true`. To cover the image ENTRYPOINT, first enable `execd` init mode; set `entrypoint = "high"` only if that process and its descendants should retain baseline (`opensandbox-system`) treatment. Existing launch clients need no changes. Implementing PRs should align the optional diagnostic and capability response fields with `specs/execd-api.yaml` and its consumers; this draft changes no executable or public schema.
