---
title: Resource-Aware Scheduling for Fast Sandbox
description: Configure fast-sandbox pool scheduling — LegacySlot slot accounting and ResourceAware oversubscription with per-sandbox limits, pool-owned overcommit ratios, admission, enforcement, and operations.
---

# Resource-Aware Scheduling for Fast Sandbox

Every fast-sandbox sandbox is admitted into a [SandboxPool](/architecture/fast-sandbox/scheduling) of pre-warmed Fastlet pods. The pool's `spec.sandboxResources` field selects one of two scheduling modes, which decide how resources are accounted, admitted, and enforced for every sandbox the pool produces. The mode is a property of the pool — nothing to enable on the OpenSandbox server.

| | `LegacySlot` | `ResourceAware` |
| --- | --- | --- |
| Accounting | Fixed slots, one declared shape | Per-sandbox limits + pool-owned overcommit ratios |
| Sandbox shapes | All identical | Heterogeneous, declared per create |
| Fits | Homogeneous fleets | Mixed small/large workloads at higher density |

## LegacySlot: slot accounting

```yaml
sandboxResources:
  cpu: "1"
  memory: 512Mi
  pids: 128
```

Omitting `mode` (the default) selects `LegacySlot`. Every sandbox in the pool receives exactly this shape, and admission is counted in slots: a Fastlet hosts at most `maxSandboxesPerPod` sandboxes. A create that declares `resourceLimits` must match the pool for every key the pool defines — mismatches are rejected with HTTP 400; otherwise the pool shape applies.

## ResourceAware: limits, ratios, and derived requests

```yaml
sandboxResources:
  mode: ResourceAware
  cpu: "1"        # default per-sandbox limits, used when a create does not override
  memory: 512Mi
  pids: 128       # pool-owned: always taken from the pool, never per request
  overcommitRatio:
    cpu: "4"
    memory: "2"
```

Both ratio fields are required, must be ≥ 1, and the profile is immutable after pool creation. The mode changes three things about how a create is processed:

1. **Limits and requests.** Each sandbox carries runtime limits (the pool defaults, or an explicit override — see [How sandboxes get their shape](#how-sandboxes-get-their-shape)). The pool derives the sandbox's **committed request** as `request = ceil(limit / overcommitRatio)` — the capacity the pool promises, as opposed to the burst ceiling the sandbox may use. Callers never declare requests; derivation through the immutable ratios keeps the pool as the single overcommit authority.
2. **Admission by ledger.** Each Fastlet tracks the sum of derived requests of its live sandboxes against a capacity of `defaultRequest × maxSandboxesPerPod`. A create is admitted only if `committed + request ≤ capacity`; memory-pressure signals (high watermark, PSI, OOM events, host pressure) block admission independently.
3. **Host enforcement.** On Firecracker pools, each sandbox gets a cgroup leaf with `cpu.max` from its **limit**, `cpu.weight` from its derived **request**, and `pids.max` from the pool, under a shared aggregate VM memory budget. The guest machine is derived from the same limits (`vCPUs = ceil(milliCPU / 1000)`, `memSizeMiB = ceil(bytes / MiB)`).

### Worked example

Pool limits `2` CPU / `5Gi`, ratios `8` / `1.25`, `maxSandboxesPerPod: 10`:

- Default request: `2000m / 8 = 250m` CPU, `5Gi / 1.25 = 4Gi` memory.
- Commitment capacity per Fastlet: `2500m` CPU / `40Gi` memory.
- A create declaring `4 vCPU / 8Gi` derives a request of `500m / 6.4Gi` and is admitted only if the ledger has room.

## How sandboxes get their shape

| Create path | `LegacySlot` | `ResourceAware` |
| --- | --- | --- |
| Create with `resourceLimits` | Must equal the pool profile | Forwarded as the sandbox's limits; `cpu`/`memory` validated, other keys (e.g. `pids`) stay pool-owned |
| Create without `resourceLimits` | Pool shape | Pool default limits |
| [Template](/architecture/fast-sandbox/templates) create | Pool shape | The template's recorded machine shape is forwarded |
| Snapshot restore | Pool shape | The snapshot's recorded source limits are replayed; caller `resourceLimits` must match them (400 on mismatch) |

Notes per path:

- Template creates reject caller `resourceLimits`; the shape is fixed by the template's golden image, and on a `ResourceAware` pool that recorded shape is what FastPath receives.
- A template or restore whose shape exceeds the pool's per-sandbox budget is rejected with a permanent **400** — the same shape cannot fit this pool, so retrying will not help.
- When every Fastlet's ledger is committed, or memory pressure blocks admission, creates return a retryable **429**. Pool scaling observes pending demand through the same resolution and grows the fleet between `capacity.poolMin` and `capacity.poolMax`.

## Choosing overcommit ratios

Ratios express runtime limit per unit of committed request. The examples above illustrate accounting, not recommended production values.

- Tune CPU independently from concurrent utilization, throttling, and command latency.
- If memory-demand overlap is unknown, or all VMs must reach their limits simultaneously, start with `overcommitRatio.memory: "1"` and budget for guest/VMM overhead, charged caches, and headroom.
- Validate higher memory ratios against overlapping peaks, regrowth after idle, snapshots, and available Pod/node capacity. With the default shape and slot count fixed, a higher ratio reduces derived commitments and the associated shared VM runtime budget.

FPR is enabled by default on newly built Native and OCI / OverlayBD templates; existing artifacts retain their recorded configuration. It returns guest-freed pages while balloon statistics polling remains off. It does not evict file caches, release static commitments, or prevent concurrent active-memory OOM.

The [matched November FPR benchmark](https://github.com/opensandbox-group/OpenSandbox/blob/main/benchmark/FPR-report.md) measured **93.7% / 93.9% lower idle VMM PSS** after a 3 GiB allocation/free workload in Native / OCI, but only **43.5% / 45.0%** after Maven. Default-watermark dual-VM admission recovered in about **13 s / 14 s**. Both paths passed regrowth, pause/resume, and Full cross-node restore; both still reached OOM when admitted VMs concurrently attempted 6 GiB inside a 4 GiB shared VM budget. The fixed 4× memory ratio establishes a test condition, not a safe production range.

Base admission decisions on shared cgroup `memory.current`/`memory.max`, watermark state, and OOM events alongside the commitment ledger. Per-VMM PSS can fall while charged caches keep admission blocked.

## Rollout and rollback

The pool profile is the switch — there is no server-side flag.

- **Roll out** by deploying a new pool with `mode: ResourceAware` (`sandboxResources` is immutable, so a profile change is a new pool) and routing creates to it through `extensions.poolRef` or the server's `fastpath_resource_pool` default.
- **Roll back** by pointing creates at a `LegacySlot` pool; existing sandboxes keep the shapes they were admitted with.

The PR integration lane runs the `ResourceAware` stack by default; the nightly run keeps `LegacySlot` coverage, so both profiles stay under test.

## Monitoring

`SandboxPool.status.resources` reports request-based commitment accounting:

```bash
kubectl -n opensandbox-dataplane get sandboxpool my-resourceaware-pool \
  -o jsonpath='{.status.resources}'
# {"capacity":{"cpu":"2000m","memory":"8Gi"},"committed":{"cpu":"750m","memory":"1536Mi"},"free":{"cpu":"1250m","memory":"..."}}
```

`capacity` is the pool's commitment budget, `committed` the sum of derived requests of live and in-flight sandboxes, and `free` what remains for new admissions — schedulable commitment, not host idle resources.

Each Sandbox carries the resolved shape as annotations:

```bash
kubectl -n opensandbox-dataplane get sandbox <id> \
  -o jsonpath='{.metadata.annotations.fast-sandbox\.io/resolved-request-cpu}'
```

Keys: `fast-sandbox.io/resource-policy-hash`, `fast-sandbox.io/resolved-request-{cpu,memory}`, `fast-sandbox.io/resolved-limit-{cpu,memory}`.

Fastlets expose Prometheus metrics: `fast_sandbox_fastlet_admission_total` (admission results by reason), `fast_sandbox_fastlet_admission_slots` (slot accounting), `fast_sandbox_fastlet_resource_admission_state` (resource-aware admission state and blocking reason), plus per-sandbox and pool cgroup gauges (`memory_bytes`, `memory_limit_bytes`, `oom_kills_total`, `memory_psi_some_avg10`, CPU usage and throttling). Alert on `free` approaching zero and on non-OPEN resource admission states. Firecracker native FPR counters require separate metrics configuration; PSS drops are not reclaimed-byte counters, and missing failure counters do not mean zero failures.

## Scope and compatibility

- Per-sandbox limits cover CPU and memory. `pids` is pool-owned; `disk` sizes a template's rootfs and is not a runtime limit.
- The accounting model is enforced end to end on Firecracker; other fast-sandbox runtimes inherit the model with runtime-specific enforcement.
- Existing `LegacySlot` pools require no migration — both profiles coexist as separate pools.
- Full design details, including admission invariants and rejected alternatives: [OSEP-0026](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0026-resource-oversubscription-accounting-for-fast-sandbox.md) and fast-sandbox's [resource-aware overcommit placement design](https://github.com/opensandbox-group/fast-sandbox/blob/main/docs/design/resource-aware-overcommit-placement.md).
