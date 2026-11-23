---
title: Fast Sandbox Performance
description: ACK measurements of the fast-sandbox integration — serial and concurrent creates, request latency, live snapshot, and same-node pause/resume — plus the OSEP-0023 credential-bound interception cost and recommended connection budgets.
---

# Performance

ACK (Alibaba Cloud Container Service for Kubernetes) is Alibaba Cloud's managed Kubernetes service. This page reports measurements collected on a two-node ACK cluster on 2026-10-08: serial and concurrent creates, request latency, live snapshot, and same-node pause/resume through the OpenSandbox SDK. Runtime logs provide phase timings alongside the SDK results. These are engineering measurements, not a production latency guarantee.

The warm create path admits work into pre-warmed capacity (see [Scheduling](/architecture/fast-sandbox/scheduling)) and restores a snapshot instead of booting a kernel (see [Firecracker](/architecture/fast-sandbox/firecracker)). Pause/resume uses the submit-and-converge mutation path (see [High Availability](/architecture/fast-sandbox/ha)). Capacity, runtime work, and the readiness boundary still determine the observed latency and success rate.

## How to read these numbers

- **SDK readiness** includes the Python SDK, lifecycle server, FastPath, ingress gateway, and guest `/ping` 200. Runtime creation ends earlier.
- **Warm creates** have template artifacts cached and Fastlet capacity prepared. First-use artifact delivery is a separate cost.
- **Sample counts and failures** are reported with latency. Successful-attempt percentiles do not describe failed or timed-out requests, or prove throughput.
- **Batch differences** matter: client resources and placement, and artifact-store persistence changed between the initial serial baseline and later runs. The two batches are reported separately and are not a controlled A/B comparison.

[Download the raw SDK report](/benchmarks/fast-sandbox-ack-2026-10-08.json) for individual samples, failures, runtime phases, image digests, and settings.

## Environment

| Dimension | ACK environment (2026-10-08) |
|---|---|
| Cluster | Two ACK nodes, Kubernetes `1.36.2-aliyun.1`, containerd 2.1.9 |
| CPU | Per node: AMD EPYC 9T95, 192 CPUs exposed, 191.45 allocatable |
| Memory | Approximately 742 GiB allocatable per node |
| OS / kernel | ContainerOS/Lifsea, `5.10.134-19.6.1.lifsea8.x86_64` |
| StateRoot | 50 GiB XFS `reflink=1` loop filesystem per node, backed by an ext4 ESSD data disk |
| Pool | Five Fastlets, four slots each: 20 slots |
| Snapshot workload | Alpine 3.19 + execd, 1 vCPU, 2 GiB memory, 2 GiB rootfs |
| Artifact store | RustFS on a persistent hostPath on one node during the later runs |
| Firecracker | 1.16.1 |

The SDK deployment uses server, controller, and runtime agent `release-1.1.1-rc.1`. Its derived Fastlet image adds GNU coreutils `cp` 9.11; working reflink support requires both the filesystem and copy tool. The XFS startup rejection change was not deployed during measurement. The template artifact digest is `d78575470094c6fe1a2da2a71b7578971dff9e77cd5381dee236dd53e01b54cb`, cached on both nodes. SDK version: `1.1.1rc2.dev106+gc7dc78a4.d20261008`.

## Create

### SDK creates

Each SDK create run has two untimed warmups and 100 timed attempts, with template artifacts already cached. Timing starts before `Sandbox.create_from_template()` and ends at guest `/ping` 200 through the gateway. Each sandbox is killed after readiness. Concurrent batches have a five-second reclamation pause; deletion and batch pauses are excluded from individual latency. The later serial and concurrent runs retain the same pool configuration.

The in-cluster client disables metrics, uses a ten-second HTTP request timeout, and polls readiness every 10 ms (SDK default: 200 ms). Percentiles use the nearest-rank method. Client differences between the initial serial baseline and later comparison runs are noted below.

| Measurement | Success | p50 | p90 | p99 | Mean |
|---|---:|---:|---:|---:|---:|
| SDK create, initial serial baseline | 100/100 | **64.0 ms** | 93.8 ms | 108.3 ms | 70.6 ms |
| SDK create, serial repeat alongside concurrency runs | 100/100 | 77.9 ms | 105.3 ms | 112.5 ms | 78.2 ms |
| SDK create, concurrency 10 | 100/100 | 102.7 ms | 120.1 ms | 125.4 ms | 102.4 ms |
| SDK create, concurrency 20 | 51/100 | 136.7 ms | 163.0 ms | 197.0 ms | 134.2 ms |

The initial 100-create validation is retained as the warm serial baseline. Its exact p50 is 63.995 ms, mean 70.587 ms, and range 53.811–113.326 ms; all 100 creates reached guest readiness. It used the same cluster, template, pool shape, SDK version, timeout, and 10 ms readiness criterion. Its client Pod had a two-CPU limit with placement not explicitly pinned; the later comparison client has a four-CPU limit and runs on one selected node. RustFS persistence also changed between runs, although these warm creates used cached template artifacts.

The 77.9 ms repeat remains visible because it was collected alongside the concurrent runs with their client configuration. Both serial batches are in the raw report and are not pooled. Their difference is not a controlled experiment that isolates a host, client-CPU, or storage change.

Runtime logs from the same creation windows give the following breakdown. These include each run's two untimed warmups, so their sample counts differ from the SDK table. They stop at runtime creation, before SDK readiness:

| Load | Runtime create p50 | Rootfs preparation p50 | VM state load/resume p50 | Runtime log samples |
|---|---:|---:|---:|---:|
| Serial repeat | 50.0 ms | 2.31 ms | 0.233 ms | 102 |
| Concurrency 10 | 34.1 ms | 2.76 ms | 0.234 ms | 102 |
| Concurrency 20 | 33.5 ms | 2.63 ms | 0.225 ms | 53 |

The initial serial validation's separate runtime window has a 39.43 ms runtime-create median and 2.286 ms rootfs-preparation median across 103 creates (100 timed, two warmups, and one subsequent lifecycle create).

::: warning Saturation result
The concurrency-20 latency distribution contains **successful attempts only**. There were 48 `SandboxRateLimitException` failures (HTTP 429 / Fastlet `CapacityRejected`) and one readiness timeout with repeated gateway HTTP 503. Four test CRs also stuck in deletion with an assignment annotation/status projection conflict. Their stale placement projections were cleared to let the controller perform cleanup before the next experiment. This run does not establish reliable 20-way admission or a sustainable requests-per-second rate. The implementation cause of the admission/projection failure needs a separate fix; replacing the host disk alone does not resolve it.
:::

## Request latency

The SDK samples use one warm sandbox and 100 sequential calls per path:

| Path | Success | p50 | p90 | p99 | Mean |
|---|---:|---:|---:|---:|---:|
| Warm SDK `/ping` through the gateway | 100/100 | 0.92 ms | 1.15 ms | 5.91 ms | 1.07 ms |
| SDK `commands.run('echo ack-perf')`, through completion and stream EOF | 100/100 | 202.8 ms | 204.3 ms | 207.1 ms | 203.2 ms |

The [execd command handler](https://github.com/opensandbox-group/OpenSandbox/blob/c7dc78a4090e5de2b9119e9bd93952cae24f87bd/components/execd/pkg/web/controller/command.go) has a default 200 ms stream-close grace period; the observed approximately 203 ms is consistent with that behavior. It is not a measurement of shell process startup alone.

## Live snapshot

One live snapshot was measured:

| Phase | Measured |
|---|---:|
| Snapshot request accepted | 14.4 ms |
| Request → available snapshot (`Ready` in the SDK) | 60.63 s |
| VM frozen during dump | 11.14 s |
| Runtime artifact publication | 46.22 s |
| Artifact set | 2 GiB rootfs + 2 GiB memory |

The SDK source was healthy afterward, with its file marker and boot ID preserved. Requests were not sampled throughout its freeze, so this establishes no zero-interruption claim. The initial probe used an incorrect terminal-state check and is excluded from the timings.

## Restore from snapshot

Creating a new sandbox from a published live snapshot was not measured in this ACK run. Cached checkpoint restoration was measured as part of [same-node pause/resume](#pause-resume); template restoration is covered by [Create](#create). These operations have separate completion boundaries.

## Pause / resume

For this test only, the idle pool was restricted to one existing node, retaining five Fastlets and four slots per Fastlet. Every sample's source and destination Kubernetes node were verified through the CR's placement and the Fastlet Pod's `spec.nodeName`. All three resumes moved to a different Fastlet on that **same node** and used the node's checkpoint cache. The original pool selector was restored afterward.

| Measurement | Median | Observed range (3 samples) |
|---|---:|---:|
| SDK pause request accepted | 22.4 ms | 22.0–25.5 ms |
| Pause request → durable `Paused`, including upload and release | 61.14 s | 61.13–61.23 s |
| VM frozen during checkpoint dump | 11.15 s | 11.13–11.18 s |
| Publish approximately 4 GiB (2 GiB rootfs + 2 GiB memory) | 45.61 s | 45.61–45.63 s |
| Resume runtime creation, after cached artifacts are available | 35.49 ms | 33.64–35.50 ms |
| Resume rootfs preparation | 2.92 ms | 2.64–3.26 ms |
| SDK same-node resume → guest `/ping` 200 | **62.66 s** | **58.29–65.16 s** |

All three samples preserved the filesystem marker, tmpfs marker, boot ID, background PID, and process start time. The sandbox identity also stayed the same. No checkpoint artifact-delivery attempt appears in the corresponding Fastlet logs. Rootfs checkpoint cloning took 0.88–0.94 ms with zero full-copy time. Three samples establish this observed range, not a reliable p99.

::: warning Same-node resume remains slow
Cached runtime restoration is tens of milliseconds, but usable SDK resume is approximately a minute in this deployment. Runtime logs publish the route immediately after restoration, leaving most elapsed time in endpoint/guest availability and client waiting. These measurements do not isolate which network, gateway, endpoint-resolution, or retry mechanism causes that gap. The gap cannot be explained by copying a 2 GiB rootfs or fetching a checkpoint onto another node. Do not substitute the runtime number for SDK resume.

The probe used a four-minute resume budget and ten-second HTTP request timeout; the SDK's default resume budget is 30 seconds. The reported result includes actual readiness waiting and does not skip health checks.
:::

## Artifact delivery (DART P2P)

Not verified in this ACK run. Template artifacts were already cached on both nodes, and same-node resumes used the checkpoint cache. Cold delivery latency, origin/peer block counts, and P2P efficiency require separate measurements.

## Boot characteristics

Cold kernel boot was not verified in this ACK run. Template creates restore `vmstate.snap` instead of booting a kernel; their measured VM state load/resume phases appear under [Create](#create).

## Interpreting the results

XFS reflink support and GNU `cp` keep warm rootfs preparation in the millisecond range: the initial serial runtime window measured a 2.286 ms median. This measures the storage/copy phase, not total SDK readiness.

Concurrency changes admission contention; durable pause includes dumping, hashing, and uploading a checkpoint; SDK readiness includes endpoint resolution, HTTP timeouts, and retries. The saturation failures under [Create](#create) are control-plane evidence, not a host-only explanation. The same-node resume gap also remains unresolved. A warm `/ping`, completed command stream, runtime restore, and SDK resume have different completion boundaries.

## Credential-bound interception cost (OSEP-0023)

This section answers two questions about the experimental Fast Sandbox `credential-bound` interception mode (see [Egress](/architecture/network/egress)): how much the mode adds to sandbox creation latency, and what the per-subject and process connection budgets should be. The same bench ran in two KVM integration environments (`scripts/fast-sandbox-env`): CI workflow run [44396818387](https://github.com/opensandbox-group/OpenSandbox/actions/runs/44396818387) on the self-hosted `kvm` runner (2026-11-18), and a rerun on a shared bare-metal host (2026-11-19) with the CI parameters (set A) and with larger samples (set B). Neither environment is the ACK cluster above, so absolute values are not comparable with the earlier sections. The two environments also differ in hardware, kernel and background load; compare each environment's two legs, not absolute values across environments.

### Summary

| Measurement | CI | Bare metal, set A | Bare metal, set B |
|---|---:|---:|---:|
| M3 bytes per held bound connection, `anon` / `memory.current` | 158,100 / 158,847 | 158,582 / 165,483 | 158,231 / 165,012 |

What the data supports:

- **Memory.** An idle bound connection costs 158–166 KB of egress memory. The `anon` slope agreed within 0.3% across two hosts, two kernels and up to 4,000 connections; `memory.current`, the value the container limit enforces, was about 4% higher on the bare-metal host. The budgets below use the higher value.
- **Total memory.** `all` mode costs about 165 KB of `memory.current` per held connection whether or not the host is bound; a `credential-bound` passthrough connection costs about 35 KB. With 5% of connections bound, `credential-bound` needs about 25% of the per-connection memory of `all` (see M3x).
- **Not measured.** Active transfers in M3/M3x.

### Method

The `bench-credential-bound` action of [`integration-env.sh`](https://github.com/opensandbox-group/OpenSandbox/blob/main/scripts/fast-sandbox-env/integration-env.sh) runs after the verify actions in each integration leg. The default leg leaves `OPENSANDBOX_EGRESS_FSB_INTERCEPTION_MODE` unset (interception `all`); the second leg sets `credential-bound`. Both legs use the same pool, template, origin and client. Each leg is a separate environment bring-up, and the legs run one after the other on the same host without interleaving, so a cross-leg difference also contains bring-up-to-bring-up variation. Each measurement writes a JSON report with closed-vocabulary fields only: no credential value, digest, sandbox hostname or subject UID.

| Measurement | What is measured | Legs |
|---|---|---|
| M1 create latency | [`create-bench.py`](https://github.com/opensandbox-group/OpenSandbox/blob/main/scripts/fast-sandbox-env/create-bench.py): rounds of creates at concurrency 10 from the native template, each create carrying a network policy so that every sandbox registers an egress subject. Timing ends at guest `/ping` 200 through the gateway. | Both |
| M2 TLS connect latency | From inside the guest to the persistent HTTP/1.1 TLS test origin used by the credential-bound verify: a new TCP connection and TLS handshake per sample, measured client-side. One request per sample confirms which path was taken. | Both, per path |
| M3 memory per connection | Idle bound HTTP/1.1 keep-alive connections held from 4 sandboxes on one Fastlet Pod, at cumulative steps of K. The egress container's cgroup `memory.current` and `memory.stat` `anon` are read after each step. Bytes per connection are fitted by least squares over the raw points, including the K = 0 baseline. | `credential-bound` only |
| M4 CPU | Passthrough-dominated load: short HTTPS requests (new connection, one request, close) from an unbound sandbox plus 5% as many from a bound sandbox, on one Fastlet Pod. Reported as the egress container's cgroup `cpu.stat` `usage_usec` delta, minus the idle rate from a 30-second window before the load. | Both |

| Parameter | CI and bare-metal set A | Bare-metal set B |
|---|---|---|
| M1 rounds × creates (concurrency) | 3 × 200 (10) | 5 × 400 (10) |
| M2 samples per path | 200 | 1,000 |
| M3 steps K; per-subject budget in the M3 render | 500, 1000, 2000; 1024 | 500, 1000, 2000, 4000; 2048 |
| M4 unbound + bound requests | 1,900 + 100 | 4,750 + 250 |

M2 paths:

| Path | Leg | Sandbox | Expected evidence |
|---|---|---|---|
| `all_unbound` | default (`all`) | no Vault | MITM leaf, no credential (full decrypt) |
| `all_bound` | default (`all`) | Vault bound to the origin host | MITM leaf, injected credential |
| `cb_bound` | `credential-bound` | Vault bound to the origin host | MITM leaf, injected credential (decrypt) |
| `cb_unbound` | `credential-bound` | no Vault | origin leaf, no credential (opaque passthrough) |

The guest client compares each sample's presented leaf certificate digest with the origin's own leaf, because origin and MITM leaves chain to the same test CA. It also reports which credential the origin observed. A path is reported `INVALID` instead of a number when any sample contradicts the expected evidence. Warmup samples (10 per path) are discarded. Percentiles use the nearest-rank method.

M1 writes no Vault: `create-bench.py` has no Vault step. The cost of binding a credential to a created sandbox is reported separately as the Vault relay acknowledgement times (`vault_bind_ack_ms`) in the M2 and M3 reports, and the per-connection cost of a bound sandbox is covered by M2 and M3.

M3 runs on a bench-only Pool render that raises the egress container memory limit to 2Gi and `OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY` to the per-subject budget above, so that the largest step fits. The default render and the verify lanes are unchanged, and the default Pool is re-applied after M3. If a step cannot be reached, the report keeps every point collected so far, the largest K reached, and the closed failure kinds. The step is never dropped from the report, and only steps whose held count equals the target enter the fit. An egress container restart or OOM kill also stops the series and is recorded.

The TLS origin fixture holds one socket per held connection, so M3 also needs the origin container's open-file soft limit well above the largest K. The container inherits the Docker daemon's default. On the bare-metal host that default was 1024, and the first `credential-bound` set A attempt reported M3 `PARTIAL`: 1,896 of 2,000 connections held and 876 alive at release. The origin's soft limit was raised to 65536 and the whole set was rerun; the `PARTIAL` attempt is not used on this page.

### Environment

Each leg's `environment.json` records: leg and interception mode, recording time, OpenSandbox and fast-sandbox commits, pool name, mode, `poolMin` and `maxSandboxesPerPod`, OverlayBD profile, host kernel, CPU model and count, host memory, Kubernetes version, egress image, and the egress container resources as deployed. Its `topology` field is a fixed string; the bare-metal topology below comes from inspecting the host.

| Dimension | CI | Bare metal |
|---|---|---|
| Date | 2026-11-18 | 2026-11-19 |
| Code | Workflow run [44396818387](https://github.com/opensandbox-group/OpenSandbox/actions/runs/44396818387): pull request merge commit `b957c8ab` of head `d3123e2a` (#2924 through `18ee344e`, plus a temporary commit that enabled the bench); fast-sandbox `37421d95` | Commit `7f2cd294` of the superseded pull request #2924; fast-sandbox `37421d95`. Compared with the CI run, the bench client also counts a non-200 or non-origin response as a failed sample, and the process TLS budget default is 32768 instead of 131072; no M3 step comes near the process budget under either default |
| Topology | kind (Kubernetes v1.31.0) on the self-hosted `kvm` runner | kind (Kubernetes v1.31.0) on a bare-metal host shared with unrelated workloads. The bench used its own Docker daemon and containerd in a private network namespace, inside one cgroup capped at 24 CPUs and 64 GiB; CPUs and NUMA nodes were not pinned |
| Host CPU | Intel Xeon Platinum 8163 @ 2.50GHz, 96 CPUs | 2 × Intel Xeon Gold 6130 @ 2.10GHz, 64 CPUs |
| Host memory / kernel | about 503 GiB / `6.6.102-5.600.al8.x86_64` | about 251 GiB / `5.14.0-570.12.1.el9_6.x86_64` |
| Pool | `firecracker-egress-resource-aware-pool`, ResourceAware, `poolMin` 2, `maxSandboxesPerPod` 8, native template | Same; `OCI_STACK=0` because the kernel has no ublk |
| Egress container resources (M1, M2, M4) | requests 100m CPU / 64Mi, limits 500m CPU / 256Mi | Same |
| Egress container resources (M3 bench render) | 2Gi memory limit, per-subject budget 1024 | 2Gi memory limit, per-subject budget 1024 (set A) or 2048 (set B) |

The bare-metal run used a wrapper outside the repository for isolation and for an outbound proxy during image builds. The bench code, its parameters and the Pool renders were not modified. During the four measurement windows the bench cgroup recorded no OOM kill and no CPU throttling at the cgroup level, and the host had at least 121 GiB of available memory.

### Results

#### M3 egress memory

`credential-bound` leg, fits over all points (bytes per connection):

| Run | Steps K | `anon` slope (r²) | `memory.current` slope (r²) | `memory.current` at K = 0 |
|---|---|---:|---:|---:|
| CI | 500, 1000, 2000 | 158,100 (0.9999) | 158,847 (0.9999) | 66,195,456 |
| Bare metal, set A | 500, 1000, 2000 | 158,582 (0.9999) | 165,483 (0.9998) | 69,877,760 |
| Bare metal, set B | 500, 1000, 2000, 4000 | 158,231 (1.0000) | 165,012 (1.0000) | 70,160,384 |

CI raw points:

| K held | `memory.current` | `anon` |
|---:|---:|---:|
| 0 | 66,195,456 | 65,708,032 |
| 500 | 143,253,504 | 142,233,600 |
| 1000 | 224,583,680 | 223,834,112 |
| 2000 | 383,152,128 | 381,063,168 |

Bare-metal set B raw points:

| K held | `memory.current` | `anon` | `kernel` |
|---:|---:|---:|---:|
| 0 | 70,160,384 | 65,732,608 | 3,686,400 |
| 500 | 149,352,448 | 142,082,048 | 6,787,072 |
| 1000 | 232,583,168 | 222,195,712 | 9,940,992 |
| 2000 | 399,998,976 | 381,128,704 | 16,166,912 |
| 4000 | 728,399,872 | 697,409,536 | 28,667,904 |

Every run reached every step with all targeted connections held, no egress container restart and no OOM kill, and every held connection served one more request at release. The `anon` slope agrees within 0.3% across the three runs. On the bare-metal host, the cgroup `kernel` counter grew by about 6.2 KB per connection, which accounts for most of the gap between its `memory.current` and `anon` slopes. That is kernel memory charged to the container, not mitmproxy heap; the CI run's `kernel` counter is not compared here. The four-decimal r² of set B rounds to 1.0000; the unrounded values are 0.99998 (`anon`) and 0.99997 (`memory.current`).

Each held connection is a decrypted client connection plus its upstream TLS connection inside mitmproxy, idle after one small request. Active transfers add buffer memory that this measurement does not include. Opaque passthrough connections are not charged to either budget and are not measured here, and the default leg does not run M3.

### Recommended budgets

The per-subject default `OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY` is 32. The process default `OPENSANDBOX_EGRESS_FSB_PROCESS_TLS_CAPACITY` is 131072, and a separate change proposes lowering it to 32768. At 158–166 KB per held connection, 131072 connections need 20.7–21.7 GB of connection memory, far above the 256Mi, 2Gi and 8Gi egress memory limits considered on this page. The budgets follow from the M3 fits as below.

- **Per-subject budget** `S`: the number of concurrent decrypted connections one sandbox may hold. It is a cap, not a reservation, so the egress memory it implies is `S × sandboxes per Fastlet Pod × bytes per connection`, which only materializes if every sandbox on the Pod uses its full budget. With `maxSandboxesPerPod` sandboxes per Pod and bytes per connection `m` from M3, this worst case must stay below the egress container memory limit `L` minus the idle baseline `B`: `S × maxSandboxesPerPod × m ≤ L − B`.
- **Process budget** `P`: the total decrypted connections one egress process admits across all its subjects. Because per-subject budgets are not reservations, `P` is the bound that actually protects the container: `P × m ≤ L − B`. A `P` larger than `(L − B) / m` lets the process reach the memory limit before the budget rejects handshakes. Exhaustion of either budget terminates only new bound handshakes.
- **Consistency**: `P` should not be below `S × maxSandboxesPerPod`, or a single Pod's sandboxes cannot all reach their per-subject budget. At the default `S`, `S × maxSandboxesPerPod` is 32 × 8 = 256 in the integration environment, far below either process default, so the per-subject budget, not `P`, is what limits a Pod's decrypted connections there.
- **Headroom**: M3 measures idle connections. Keep `P × m` well below `L − B` so active transfers and Python allocator growth fit; the recommendations below leave at least a third of `L − B` unused.

`m` uses the highest `memory.current` slope measured, because the container memory limit is enforced on `memory.current`, and `B` uses the highest `memory.current` baseline. A different egress image, mitmproxy version or kernel can change both values; rerun M3 before relying on them there.

| Input | Value |
|---|---|
| Bytes per bound connection `m` | 166 KB (165,483 bytes, bare-metal set A `memory.current`, rounded up) |
| Idle baseline `B` | about 70 MB (`memory.current` at K = 0: 66–70 MB) |
| Recommended `S` | 32 (the default) |
| `L` = 256Mi (integration environment Pool, 8 sandboxes per Pod) | `(L − B) / m` ≈ 1,195; recommend `P` = 256 (`S × 8`, ≈ 42 MB) |
| `L` = 8Gi with 100 sandboxes per Pod | `S × 100` = 3,200 connections ≈ 531 MB; recommend `P` = 4096 (≈ 680 MB) |
| `L` = 8Gi, proposed process default | `P` = 32768 ≈ 5.4 GB, within `L − B` with more than a third unused |

For another Pool, set `P` to the smaller of `S × maxSandboxesPerPod` rounded up to a power of two and about two thirds of `(L − B) / m`.

Passthrough connections are not charged to either budget, but each costs about 35 KB of `memory.current` (M3x). Leave room for them in `L − B` when a Pool expects many concurrent unbound connections. In the supplemental measurements, the egress process kept the memory of 2,048 closed connections for the whole 240-second observation window, so size `L` for the peak number of concurrent connections, not the average.

### Supplemental measurements

These measurements, labelled M3x and M5–M10, cover what the first runs did not. They ran from 2026-11-20 to 2026-11-23 on the same shared bare-metal host, with the same isolation as set B: a private Docker daemon and containerd, kind, and one cgroup capped at 24 CPUs and 64 GiB. The bench is not part of this repository; its method, parameters and acceptance rules are in [`SUPPLEMENTAL.md`](https://github.com/hpliStartAgain/OpenSandbox/blob/6bf7d46e6cf2b76564b12098d6fe1fb65e2841d1/scripts/fast-sandbox-env/SUPPLEMENTAL.md) on a reproduction branch. The measurements used three frozen versions of it, listed below. Each version passed an independent code review and an independent test pass before it ran, and all summary values on this page were recomputed independently from the raw reports. Performance was not remeasured on current `main`.

| Measurements | Frozen bench version | Code under test |
|---|---|---|
| M5, M10 | [`1a0cc3e3`](https://github.com/hpliStartAgain/OpenSandbox/commit/1a0cc3e3276acd0ddabedbfa1a3989f78ae04bf6) | #2924 at `2a133535` |
| M3x, M6, M7 distributed bursts, M9 | [`ee85f9ae`](https://github.com/hpliStartAgain/OpenSandbox/commit/ee85f9ae25388852deb243002dae37f7939156f3) | same |
| M7 per-subject budget, M8 | [`0a5e50c5`](https://github.com/hpliStartAgain/OpenSandbox/commit/0a5e50c593bcacbe311563dd67f0b813298c47e4) | same |

The reproduction branch ([`6bf7d46e`](https://github.com/hpliStartAgain/OpenSandbox/tree/6bf7d46e6cf2b76564b12098d6fe1fb65e2841d1/scripts/fast-sandbox-env)) is the last frozen version rebased onto current `main`, with fixes to M7 fill accounting, refill handling and boundary-probe validation. Each mode switch rebuilds the Pool, so different modes never share an egress process. The egress CPU limit is the default 500m unless stated otherwise. The ACK create path was not measured.

#### M3x total memory

Idle held HTTP/1.1 keep-alive connections at K = 0, 500, 1000, 2000 and 4000. Each mode × target path ran on a freshly rebuilt Pool, with its own egress process, and the K = 0 baseline was checked against the fresh process. Bytes per connection, least-squares slope (r²):

| Mode | Target | Path inside egress | `anon` | `memory.current` |
|---|---|---|---:|---:|
| `all` | unbound | decrypt | 157,468 (1.0000) | 164,889 (0.9999) |
| `all` | bound | decrypt and inject | 157,389 (1.0000) | 164,750 (0.9999) |
| `credential-bound` | unbound | opaque passthrough | 27,437 (0.9993) | 34,575 (0.9995) |
| `credential-bound` | bound | decrypt and inject | 157,737 (1.0000) | 164,954 (1.0000) |

The `credential-bound` bound slope matches the 158–166 KB measured in M3. Derived from these slopes, the memory per connection for a mix with bound fraction `f` is:

| Bound fraction `f` | `all`, `memory.current` | `credential-bound`, `memory.current` | Ratio |
|---:|---:|---:|---:|
| 5% | 164,882 | 41,094 | 25% |
| 25% | 164,854 | 67,170 | 41% |
| 50% | 164,820 | 99,764 | 61% |

An earlier attempt measured the bound paths in the same egress process right after the unbound paths. Its K = 0 baseline already held the previous path's 4,000 connections' worth of memory, so the slope was underestimated. That attempt is not used.

## Reproducing

### SDK measurements

Use a source-built SDK matching the recorded version. Set `DOMAIN`, `API_KEY`, and `TEMPLATE_ID` for the target cluster without putting credentials in the report. Run from an in-cluster client to retain this network boundary:

```bash
REQUEST_TIMEOUT=10 DISABLE_METRICS=1 PING_INTERVAL_MS=10 READY_TIMEOUT_WARM=180 \
N=100 CONCURRENCY=1 BATCH_PAUSE=0 REPORT_PATH=serial.json \
  python scripts/fast-sandbox-env/create-bench.py

REQUEST_TIMEOUT=10 DISABLE_METRICS=1 PING_INTERVAL_MS=10 READY_TIMEOUT_WARM=180 \
N=100 CONCURRENCY=10 BATCH_PAUSE=5 REPORT_PATH=concurrent10.json \
  python scripts/fast-sandbox-env/create-bench.py

# Saturation diagnostic: record failures as well as successful latencies.
REQUEST_TIMEOUT=10 DISABLE_METRICS=1 PING_INTERVAL_MS=10 READY_TIMEOUT_WARM=180 \
N=100 CONCURRENCY=20 BATCH_PAUSE=5 REPORT_PATH=concurrent20.json \
  python scripts/fast-sandbox-env/create-bench.py
```

`REPORT_PATH` saves raw attempts, settings, success/failure counts, and nearest-rank percentiles. A run with failed creates returns a nonzero exit status. Save the deployed image digests, host/storage details, template digest, pool shape, and timestamped Fastlet/controller logs alongside it. The SDK run used an equivalent probe that also collected lifecycle and request samples; the report retains those original samples.

For the other measurements, retain the same SDK connection settings:

1. On one warm sandbox, time 100 sequential `sandbox.is_healthy()` calls, asserting success; then time 100 `sandbox.commands.run('echo ack-perf')` calls through stream EOF, asserting the expected output.
2. On an idle pool restricted to one node, create a fresh sandbox for each of three lifecycle samples. Before pausing, wait for CR `status.runtime.state == Ready`; the SDK's early readiness result can precede that projection. Write markers to the filesystem and tmpfs and record the boot ID and a background process's PID/start time.
3. Start the pause timer before `sandbox.pause()`. Poll `manager.get_sandbox_info(id).status.state == 'Paused'` every 200 ms; an accepted pause response alone is not a durable checkpoint.
4. Time `Sandbox.resume(id, resume_timeout=timedelta(minutes=4), health_check_polling_interval=timedelta(milliseconds=10), connection_config=config)` through its normal readiness check. Verify placement on the original node and all guest markers before killing the sandbox. Restore the pool's original selector and wait for five warm, ready Fastlets.
5. For live snapshot, time `sandbox.create_snapshot()` acceptance separately from `manager.get_snapshot(id).status.state == 'Ready'` (200 ms polling). The public snapshot API uses `Ready`; template builds use `Succeeded`. Verify the source and delete the test snapshot and sandbox.

Capture runtime dump, publish, restore, and route-publication log timestamps to distinguish checkpoint I/O from later SDK readiness. Clean up only the artifacts created by the test after their sandbox/snapshot resources are gone.

Keep hardware, kernel, Firecracker version, template digest, pool shape, client placement, and artifact-store placement fixed when comparing load shapes.

### Credential-bound interception cost

Run after `up` and the verify actions, with the same `EGRESS_INTERCEPTION_MODE` as `up`. Reports are written under `$WORK/bench/<leg>/` (`m1-create.json`, `m2-handshake.json`, `m3-memory.json`, `m4-cpu.json`, `environment.json`, `bench-summary.json`):

```bash
./scripts/fast-sandbox-env/integration-env.sh bench-credential-bound
EGRESS_INTERCEPTION_MODE=credential-bound ./scripts/fast-sandbox-env/integration-env.sh bench-credential-bound
```

In CI, the integration workflow runs the bench in both legs when the `bench` dispatch input is set or the pull request carries the `fast-sandbox-bench` label, and uploads the reports as `fast-sandbox-bench-<leg>-<attempt>` artifacts. The `BENCH_*` variables listed in the [environment README](https://github.com/opensandbox-group/OpenSandbox/blob/main/scripts/fast-sandbox-env/README.md) change sample counts, steps and the M3 render. The defaults are set A; set B on this page used:

```bash
BENCH_CREATE_ROUNDS=5 BENCH_CREATE_N=400 BENCH_HANDSHAKES=1000 \
BENCH_MEM_STEPS="500 1000 2000 4000" BENCH_FSB_TLS_CAPACITY=2048 \
BENCH_CPU_UNBOUND=4750 BENCH_CPU_BOUND=250 \
  ./scripts/fast-sandbox-env/integration-env.sh bench-credential-bound
```

Run both legs in the same environment and with the same parameters, and save each leg's reports before the next bring-up. To compare modes on a host with background load, repeat the pair of legs more than once rather than relying on one pair. Check the Docker daemon's default open-file limit before M3; see [Method](#method).

The supplemental measurements M3x and M5–M10 are not part of this repository or the CI workflow. Their entry, parameters and report format are described in [`SUPPLEMENTAL.md`](https://github.com/hpliStartAgain/OpenSandbox/blob/6bf7d46e6cf2b76564b12098d6fe1fb65e2841d1/scripts/fast-sandbox-env/SUPPLEMENTAL.md) on the reproduction branch. M8 needs the target rates chosen from an M5 run in the same environment. M9 at 4,096 subjects needs a much longer wait per tier than the default.
