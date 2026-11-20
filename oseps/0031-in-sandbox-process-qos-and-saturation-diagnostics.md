---
title: In-Sandbox Process QoS and Saturation Diagnostics
authors:
  - "@hittyt"
creation-date: 2026-11-17
last-updated: 2026-11-19
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
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Static configuration](#static-configuration)
  - [Launcher behavior](#launcher-behavior)
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

Building on [OSEP-0018](0018-execd-as-sandbox-init.md), this proposal lets a sandbox creator give `execd` a better share of CPU time under contention and make ordinary workloads more likely OOM victims. Phase 1 is opt-in, best effort, and needs neither extra Linux capabilities nor a writable cgroup. Only `execd` and an explicitly selected, `execd`-managed image ENTRYPOINT retain baseline priority. Without `execd` init mode, the bootstrap launches the ENTRYPOINT outside this policy. Non-blocking sampling on `GET /metrics` helps operators distinguish resource pressure from other causes of a slow response.

## Motivation

1. **CPU contention:** A parallel build can put dozens of runnable threads in competition with `execd`'s HTTP and WebSocket handlers. Lowering the workers' scheduling weight gives the handlers a better chance to run while CPU time is available. It does not make a shell spawned by those handlers a priority shell.
2. **Collateral OOM kills:** Linux scores individual candidates, not the combined footprint of all workers in an application. With equal `oom_score_adj`, a resident platform daemon can score higher than any one worker even when the workers collectively exhaust memory. For a memory-cgroup OOM the adjustment is scaled by the memcg limit; for an unconstrained global OOM it is scaled against system-wide pages. A shared negative adjustment (such as `-997`) is an offset, not a clamp to one: points can be negative, and an equal-scoring eligible candidate can replace the previous candidate. Neither case implies that the earliest-started process dies. See [Linux v6.6 victim scoring and selection](https://github.com/torvalds/linux/blob/v6.6/mm/oom_kill.c#L201-L350).
3. **Slow diagnostics:** `MetricController.readMetrics` calls `resources.CPUPercent(time.Second)` in the request path. A slow probe alone cannot distinguish the cost of sampling, cgroup throttling, memory reclaim, an I/O stall, or loss of connectivity.

### Goals

- Improve `execd`'s CPU share relative to managed workloads and steer OOM selection without making either a survival or latency guarantee.
- Let a creator retain baseline treatment for a trusted ENTRYPOINT, without guessing its role or exposing a priority choice to execution clients.
- Remove the per-request CPU sampling delay and expose available pressure signals without reading workload volumes.

### Non-Goals

- No hard CPU, memory, or PID isolation; no privileged setup or external server switch.
- No changes to launch requests, lifecycle hook schemas, or SDK launch methods; no dedicated emergency shell or process-management endpoint.
- No Windows scheduling policy or disk bandwidth throttling.

## Requirements

| ID | Requirement | Priority |
|---|---|---|
| R1 | With QoS disabled (the default), scheduling, OOM adjustments, resource limits and cgroup placement remain unchanged. | Must |
| R2 | With QoS enabled, managed workloads start as `standard`. Only a managed ENTRYPOINT may be configured as `high`; lifecycle hooks, commands, sessions, PTYs and isolated workloads always start as `standard`. | Must |
| R3 | Apply CPU settings and an initial OOM adjustment to the child before workload execution, including across UID changes, without requiring extra capabilities. Never decrease an inherited OOM adjustment or rely on the adjustment being locked. | Must |
| R4 | QoS does not enable, weaken or bypass OSEP-0018 hardening. | Must |
| R5 | Metrics readers share a CPU sample without sleeping in the request path. Pressure and process-state fields are optional; existing required fields retain their names and types. | Must |
| R6 | Report startup QoS support and incomplete coverage separately from observed runtime pressure. | Must |

## Proposal

| Launch path | Treatment when QoS is enabled |
|---|---|
| `execd` | Retains its inherited scheduler and OOM settings (`high` is not a priority boost). |
| Image ENTRYPOINT launched by `execd` | `standard` unless the creator explicitly selects `high`. Descendants inherit the starting settings. Use `high` only for a trusted daemon, not for a supervisor that also starts user builds. |
| Lifecycle hooks, commands, sessions, PTYs and isolated workloads | `standard`, with no per-request or per-hook override. A platform daemon that needs baseline treatment must run as the configured ENTRYPOINT, not as a hook. |
| ENTRYPOINT launched by the external bootstrap | Unchanged and outside coverage. Enable the OSEP-0018 init path (`EXECD_INIT`) to manage it. |

### Notes/Constraints/Caveats

The two tiers share one cgroup. CPU weights matter when runnable tasks compete for available CPU; after the sandbox exhausts its CPU quota, the entire group, including `execd`, waits for replenishment. Memory reclaim can stall `execd` before an OOM even occurs. There is no reserved memory or PID headroom and no guarantee that a new PTY can start.

### Risks and Mitigations

| Risk | What phase 1 can do |
|---|---|
| Many runnable workers dilute the benefit of per-task weights | `SCHED_IDLE` is an alternative for background work. For illustration, on one CPU in the same fair scheduling group, one runnable nice-0 thread competing with N nice-19 threads has nominal share `1024/(1024+15N)`. This is not a reserved share. |
| A workload changes its OOM adjustment, or inherits an adjustment already at or above the target | The launcher only sets a starting preference. The OOM killer may still select a platform daemon. |
| Runtime lacks a scheduling or procfs facility | Report the limitation and continue the workload; do not assume Docker, Kubernetes, gVisor, Kata and microVM guests implement identical semantics. |
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
```

Omitting the section or `enabled` leaves QoS off. The other values shown are defaults. Validate `entrypoint` as `standard` or `high`, `scheduler` as `other` or `idle`, `nice` in `1..19` (used with `other`), and `oom_score_adj` in `0..1000` (an explicit zero disables adjustment). Reject invalid static configuration at startup rather than silently changing the creator's choice.

### Launcher behavior

`launchManagedWith` selects the tier from the launch path, not the request payload. Add an independent QoS flag and its settings to the versioned policy shared by `execd` and `opensandbox-launcher`. Invoke the launcher for a `standard` workload even when hardening is off. A `high` ENTRYPOINT skips only the QoS adjustment. Preserve the existing hardening policy, credential handling and launch failure behavior in both tiers. Internal file API helpers act on behalf of `execd`, not as workload entry points; apply the standard policy to isolated workloads without changing the namespace helper's hardening exemption.

1. The launcher adjusts itself before executing the workload rather than racing a parent-side update against `execve`. Set both `RLIMIT_NICE` limits to zero and apply the selected scheduling policy: nice 19 by default, or `SCHED_IDLE`. This restricts an unprivileged child's ability to increase priority; no extra capability is requested.
2. For OOM steering, open `/proc/self/oom_score_adj` before an in-launcher UID switch so that a later procfs ownership change does not prevent opening it. After the requested identity/capability changes and before Landlock/seccomp, raise the adjustment only if it is below the configured nonzero target. Close the descriptor before executing the workload.
3. An unprivileged write changes the current adjustment, **not** `oom_score_adj_min`. Linux only updates that minimum on a privileged write with `CAP_SYS_RESOURCE`; the child can subsequently lower the adjustment as far as the existing minimum permits. Phase 1 neither requests that capability nor promises a lock. See [Linux v6.6 procfs write semantics](https://github.com/torvalds/linux/blob/v6.6/fs/proc/base.c#L1084-L1112).
4. If a QoS step is unavailable, report the failed setting and continue with the settings that succeeded. This fail-open behavior applies only to QoS, not to identity changes or other existing hardening checks.

### Saturation diagnostics

Replace the per-request one-second sleep with one background CPU sampler in `pkg/resources`. `GET /metrics`, `GET /metrics/watch` and OpenTelemetry read the latest sample without resetting one another's measurement window. Preserve the watch cadence. The handler does not wait for a fresh interval or promise an HTTP response deadline.

Extend `Metrics` with optional fields. Use only procfs and cgroupfs; do not probe a workload mount or read process command lines. A missing field means unavailable. In particular, PSI depends on kernel configuration, boot settings and runtime exposure. If pressure files cannot be read, omit their fields; compare successive `cpu.stat` counters when available. Do not substitute host-wide pressure for sandbox pressure on a shared host.

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

Add an optional `qos` object to `GET /v1/isolated/capabilities` using the existing layer-state vocabulary (`active`, `disabled`, `degraded`, `unsupported`) and a diagnostic message. Report configured ENTRYPOINT treatment and startup support; this is not proof that every launch applied every setting. With init mode off, report incomplete ENTRYPOINT coverage while continuing to apply QoS to managed workloads. Do not report a cgroup enforcement layer. Pressure metrics remain useful independently of whether QoS is enabled.

## Test Plan

These checks are for the implementation, not results from this proposal:

- **Configuration and routing:** Cover defaults, explicit zero, invalid TOML values, a managed ENTRYPOINT in both tiers, and a bootstrap ENTRYPOINT outside coverage. Verify hooks, commands, sessions, PTYs and isolated workloads take the standard path regardless of ENTRYPOINT selection. With QoS off, compare launches to the existing behavior.
- **Linux launcher:** With and without hardening and across a UID change, inspect initial scheduler, nice limits and OOM adjustment. An inherited adjustment of 800 must stay 800 with a target of 500. In an unprivileged fixture with a known existing minimum, verify the initial raise does not install a new minimum. Check that a high ENTRYPOINT still receives all applicable hardening; simulate an unsupported QoS operation without changing hardening behavior.
- **CPU contention:** Pin a fixed pool of busy workers and `execd` to one CPU **without a bandwidth quota**. Compare repeated QoS-off/on trials with the same probe rate and compare p95 `/ping` and `/metrics` latencies; expect improvement under contention, not a fixed deadline. In a separate quota-limited run, check for increasing throttling counters and record probe latency without a pass/fail latency threshold. Measure short `/command` launch overhead separately with hardening off before publishing performance claims.
- **Memory pressure:** Check initial adjustments independently of victim selection. In a bounded memcg fixture with individual OOM kills and workers that retain the assigned adjustment, record victim selection over repeated QoS-off/on runs. Do not require that a specific process always survives or infer protection against reclaim stalls.
- **Diagnostics:** Test cached reads by concurrent metrics/watch/telemetry consumers, v1/v2 throttle units, absent PSI, sampled process states and a bootstrap ENTRYPOINT coverage warning. Verify missing optional data preserves the existing metrics response.

## Drawbacks

Every managed standard launch may incur an extra launcher exec and policy setup when hardening is off. Hooks and interactive shells deliberately have no promotion path. Per-process policy cannot bound aggregate worker CPU, memory or PID use.

## Alternatives

Running all ENTRYPOINTs or PTYs at baseline priority would also promote user builds run through those paths. The static ENTRYPOINT exception covers a creator-owned platform daemon without asking execution clients to classify workloads.

## Future Work

Revisit per-launch or per-hook QoS selection and formalizing `/pty` in OpenAPI only if a concrete client needs them. A separate cgroup v2 design for delegated environments could consider group CPU limits, a workload memory boundary and PID headroom. Neither extension is required for phase 1; there is no phase-1 cgroup setup, configuration, capability report or acceptance test.

## Infrastructure Needed

No additional infrastructure for this proposal. Implementation measurements need Linux fixtures with controlled CPU placement, quota and memory limits.

## Upgrade & Migration Strategy

Existing sandbox images keep their scheduling behavior until the creator opts in via TOML. To cover the image ENTRYPOINT, first enable `execd` init mode; set `entrypoint = "high"` only if that process and its descendants should retain baseline treatment. Existing launch clients need no changes. Implementing PRs should align the optional diagnostic response fields with `specs/execd-api.yaml` and its consumers; this draft changes no executable or public schema.
