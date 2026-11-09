---
title: Resource-Aware Scheduling for Fast Sandbox
description: How fast-sandbox pool scheduling works — LegacySlot vs ResourceAware accounting, how to configure each pool profile, how sandboxes consume resources, and how to operate and monitor both modes.
---

# Resource-Aware Scheduling for Fast Sandbox

A fast-sandbox [SandboxPool](/architecture/fast-sandbox/scheduling) is the unit of capacity: every sandbox created through FastPath is admitted into a pre-warmed Fastlet pod that belongs to one pool. The pool's `spec.sandboxResources` field decides **how resources are accounted and enforced** for every sandbox that pool produces.

Two scheduling modes are available:

| Mode | Accounting | Per-sandbox shapes | Use it when |
| --- | --- | --- | --- |
| `LegacySlot` | Fixed **slots** — one declared shape for every sandbox | All identical | Homogeneous workloads; existing pools keep working unchanged |
| `ResourceAware` | **Per-sandbox limits + pool-owned overcommit ratios** | Heterogeneous; each sandbox carries its own limits | Mixed small/large workloads on shared Fastlets, with safe oversubscription |

The mode is a field on the pool, not a server setting: create the pool with the profile you want, and everything downstream follows. There is nothing to enable on the OpenSandbox server.

## LegacySlot: one shape, counted in slots

A `LegacySlot` pool declares a single resource shape:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: firecracker-egress-pool
  namespace: opensandbox-dataplane
spec:
  # runtime, capacity, fastletTemplate, ... omitted
  maxSandboxesPerPod: 5
  sandboxResources:
    cpu: "1"
    memory: 512Mi
    pids: 128
```

How it takes effect:

- Every sandbox in the pool gets exactly this shape — same guest CPU and memory, same PID limit.
- Admission is counted in slots: a Fastlet hosts at most `maxSandboxesPerPod` sandboxes, and a create is admitted when a slot is free.
- Callers cannot reshape a sandbox: a create request that declares `resourceLimits` must match the pool profile for every key the pool defines (mismatches are rejected with HTTP 400), and pool-default shapes are used otherwise.

Slot accounting treats every sandbox as identical. If you need sandboxes of different sizes — a small CLI sandbox next to a large build sandbox — use a `ResourceAware` pool (or one pool per shape, which multiplies pools to operate).

## ResourceAware: per-sandbox limits, pool-owned ratios

A `ResourceAware` pool declares **runtime limits** (the default per-sandbox shape) plus an **overcommit ratio** per dimension:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: my-resourceaware-pool
  namespace: opensandbox-dataplane
spec:
  # runtime, capacity, fastletTemplate, ... omitted
  maxSandboxesPerPod: 8
  sandboxResources:
    mode: ResourceAware
    cpu: "1"        # default per-sandbox runtime limits ...
    memory: 512Mi   # ... used when a create does not override them
    pids: 128
    overcommitRatio:
      cpu: "4"      # compressible: 4 sandboxes worth of CPU limit per core
      memory: "2"   # conservative: guest RAM is backed by host pages
```

The chain that makes this work:

1. **Limits in, requests derived.** Each sandbox carries runtime limits (the pool defaults, or an explicit shape — see [How sandboxes get their shape](#how-sandboxes-get-their-shape)). FastPath derives the sandbox's **committed request** as `request = ceil(limit / overcommitRatio)` — the shape the pool promises to keep available, as opposed to the runtime ceiling the sandbox may burst to.
2. **The Fastlet keeps a ledger.** Every Fastlet tracks the sum of derived requests of its live sandboxes against a commitment capacity of `defaultRequest × maxSandboxesPerPod`. A create is admitted only if `committed + request ≤ capacity`, and memory-pressure signals (high watermark, PSI, OOM events, host pressure) block admission independently of the static ledger.
3. **The host enforces both numbers.** On Firecracker pools each sandbox gets a cgroup leaf with `cpu.max` from its **limit**, `cpu.weight` from its derived **request**, and `pids.max` from the pool — under a shared aggregate VM memory budget sized for the whole pool. The guest machine itself is derived from the limits (`vCPUs = ceil(milliCPU / 1000)`, `memSizeMiB = ceil(bytes / MiB)`).

Requests are **never caller-owned**: they are always derived from limits through the pool's immutable ratios. That keeps exactly one overcommit authority — the pool — no matter which client declares which shape.

### Worked example

Pool limits `2` CPU / `5Gi`, ratios `8` / `1.25`, `maxSandboxesPerPod: 10`:

- Default request per sandbox: `2000m / 8 = 250m` CPU, `5Gi / 1.25 = 4Gi` memory.
- Commitment capacity per Fastlet: `250m × 10 = 2500m` CPU, `4Gi × 10 = 40Gi` memory.
- A sandbox that declares `4 vCPU / 8Gi` derives a request of `500m / 6.4Gi` and is admitted only if the ledger has room for it.

## How sandboxes get their shape

Three create paths exist; the pool mode decides how resources are resolved in each.

| Create path | `LegacySlot` pool | `ResourceAware` pool |
| --- | --- | --- |
| Image / snapshot create with `resourceLimits` | Must equal the pool profile (400 otherwise) | Forwarded as the sandbox's limits; `cpu`/`memory` are validated, other keys (e.g. `pids`) stay pool-owned |
| Image / snapshot create without `resourceLimits` | Pool shape | Pool default limits |
| [Template](/architecture/fast-sandbox/templates) create (`templateId`) | Pool shape | The **template's recorded machine shape** is forwarded as the sandbox's limits |
| Snapshot restore | Pool shape | The **snapshot's recorded source limits** are replayed; caller-supplied `resourceLimits` must match the recorded values (400 on mismatch) |

Template-mode creates reject caller-declared `resourceLimits` — the workload shape is fixed by the template's golden image, and on a `ResourceAware` pool that recorded shape is what FastPath receives. A template whose recorded shape exceeds the pool's per-sandbox budget is rejected at create time with a permanent **400**.

### Capacity exhaustion is retryable, misconfiguration is not

| Situation | HTTP result | Meaning |
| --- | --- | --- |
| Limits exceed the pool's per-sandbox budget (derived from `maxSandboxesPerPod`) | **400** `InvalidArgument` | Permanent — the shape cannot fit this pool; retrying the same request will not help |
| Every Fastlet's ledger is committed, or memory pressure blocks admission | **429** `ResourceExhausted` | Temporary — retry, or scale the pool (`capacity.poolMax`) |

Pool scaling participates in the same accounting: the controller observes pending demand through the same resolution, packs it best-fit across Fastlets, and scales the fleet between `capacity.poolMin` and `capacity.poolMax` when the budget allows.

## Choosing overcommit ratios

The ratios express how much runtime limit you sell per unit of committed request. They are declared once per pool and immutable — changing them means deploying a new pool.

- **CPU compresses well** (throttling degrades gracefully; guests are idle most of the time): ratios of 4–8× are common.
- **Memory is the hard backstop** (Firecracker guest pages are backed by host pages and guests cannot swap): keep ratios conservative — 1.25–2×.
- The derived request must stay representable: ratios must be ≥ 1, and limits must be ≥ `10m` CPU.

Oversubscription trades admission-time guarantees for density: a pool with ratio `8` CPU packs eight `1`-CPU sandboxes per committed core, but if all of them saturate their limits simultaneously they share the Fastlet's real CPU through cgroup weights. Memory overcommit without a reclaim backstop is how Fastlets OOM — hence the lower ratio and the pressure-aware admission.

## Rollout and rollback

The pool profile is the switch — there is no server-side flag.

- **Roll out** by deploying a new pool with `mode: ResourceAware` (the `sandboxResources` profile is immutable; reuse a name only when intentionally replacing the pool) and route creates to it via `extensions.poolRef` or the server's `fastpath_resource_pool` default.
- **Roll back** by pointing creates at a `LegacySlot` pool, or pinning the previous server version. Existing sandboxes keep the shapes they were admitted with.
- On the PR integration lane (self-hosted KVM) the stack runs `ResourceAware` by default and the nightly run keeps `LegacySlot` coverage — both profiles stay under test.

## Monitoring

`SandboxPool.status` exposes a resource summary in the pool's own accounting:

```bash
kubectl -n opensandbox-dataplane get sandboxpool my-resourceaware-pool \
  -o jsonpath='{.status.resources}'
# {"capacity":{"cpu":"2000m","memory":"8Gi"},"committed":{"cpu":"750m","memory":"1536Mi"},"free":{"cpu":"1250m","memory":"..."}}
```

- `capacity` — the pool's commitment budget (derived requests across ready Fastlets).
- `committed` — the sum of derived requests of live and in-flight sandboxes.
- `free` — what remains for new admissions. This is schedulable commitment, not host idle CPU or memory.

Each Sandbox carries the resolved shape on annotations for per-sandbox debugging:

```bash
kubectl -n opensandbox-dataplane get sandbox <id> \
  -o jsonpath='{.metadata.annotations.fast-sandbox\.io/resolved-request-cpu}'
kubectl -n opensandbox-dataplane get sandbox <id> \
  -o jsonpath='{.metadata.annotations.fast-sandbox\.io/resolved-limit-cpu}'
```

Fastlet pods also export admission and pressure metrics (committed vs capacity, memory watermark/PSI states, OOM events) on their Prometheus endpoint — alert on the ledger approaching capacity and on pressure-state transitions.

## Limitations and compatibility

- Per-sandbox limits cover **CPU and memory** only. `pids` stays pool-owned; `disk` is a template build dimension (rootfs size), not a runtime limit.
- `ResourceAware` pools are validated end to end on Firecracker; other fast-sandbox runtimes inherit the accounting model with runtime-specific enforcement.
- Existing `LegacySlot` pools require no migration — they keep working unchanged, and both profiles can coexist in one cluster as separate pools.
- The full design, including the admission ledger, pressure states, and rejected alternatives, is documented in [OSEP-0026](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0026-resource-oversubscription-accounting-for-fast-sandbox.md) and fast-sandbox's [resource-aware overcommit placement design](https://github.com/opensandbox-group/fast-sandbox/blob/main/docs/design/resource-aware-overcommit-placement.md).
