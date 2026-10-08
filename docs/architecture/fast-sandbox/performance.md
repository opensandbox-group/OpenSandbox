---
title: Fast Sandbox Performance
description: ACK measurements of the fast-sandbox integration — serial and concurrent creates, request latency, live snapshot, and same-node pause/resume.
---

# Performance

ACK (Alibaba Cloud Container Service for Kubernetes) is Alibaba Cloud's
managed Kubernetes service. This page reports measurements collected on a
two-node ACK cluster on 2026-10-08: serial and concurrent creates, request latency, live snapshot,
and same-node pause/resume through the OpenSandbox SDK. Runtime logs provide
phase timings alongside the SDK results. These are engineering measurements,
not a production latency guarantee.

The warm create path admits work into pre-warmed capacity (see
[Scheduling](/architecture/fast-sandbox/scheduling)) and restores a snapshot
instead of booting a kernel (see [Firecracker](/architecture/fast-sandbox/firecracker)).
Pause/resume uses the submit-and-converge mutation path (see
[High Availability](/architecture/fast-sandbox/ha)). Capacity, runtime work,
and the readiness boundary still determine the observed latency and success rate.

## How to read these numbers

- **SDK readiness** includes the Python SDK, lifecycle server, FastPath,
  ingress gateway, and guest `/ping` 200. Runtime creation ends earlier.
- **Warm creates** have template artifacts cached and Fastlet capacity prepared.
  First-use artifact delivery is a separate cost.
- **Sample counts and failures** are reported with latency. Successful-attempt
  percentiles do not describe failed or timed-out requests, or prove throughput.
- **Batch differences** matter: client resources and placement, and artifact-store
  persistence changed between the initial serial baseline and later runs.
  The two batches are reported separately and are not a controlled A/B comparison.

[Download the raw SDK report](/benchmarks/fast-sandbox-ack-2026-10-08.json) for
individual samples, failures, runtime phases, image digests, and settings.

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

The SDK deployment uses server, controller, and runtime agent
`release-1.1.1-rc.1`. Its derived Fastlet image adds GNU coreutils `cp` 9.11;
working reflink support requires both the filesystem and copy tool. The XFS
startup rejection change was not deployed during measurement. The template
artifact digest is
`d78575470094c6fe1a2da2a71b7578971dff9e77cd5381dee236dd53e01b54cb`, cached on
both nodes. SDK version: `1.1.1rc2.dev106+gc7dc78a4.d20261008`.

## Create

### SDK creates

Each SDK create run has two untimed warmups and 100 timed attempts, with template
artifacts already cached. Timing starts before `Sandbox.create_from_template()`
and ends at guest `/ping` 200 through the gateway. Each sandbox is killed after
readiness. Concurrent batches have a five-second reclamation pause; deletion
and batch pauses are excluded from individual latency. The later serial and
concurrent runs retain the same pool configuration.

The in-cluster client disables metrics, uses a ten-second HTTP request timeout,
and polls readiness every 10 ms (SDK default: 200 ms). Percentiles use the
nearest-rank method. Client differences between the initial serial baseline and
later comparison runs are noted below.

| Measurement | Success | p50 | p90 | p99 | Mean |
|---|---:|---:|---:|---:|---:|
| SDK create, initial serial baseline | 100/100 | **64.0 ms** | 93.8 ms | 108.3 ms | 70.6 ms |
| SDK create, serial repeat alongside concurrency runs | 100/100 | 77.9 ms | 105.3 ms | 112.5 ms | 78.2 ms |
| SDK create, concurrency 10 | 100/100 | 102.7 ms | 120.1 ms | 125.4 ms | 102.4 ms |
| SDK create, concurrency 20 | 51/100 | 136.7 ms | 163.0 ms | 197.0 ms | 134.2 ms |

The initial 100-create validation is retained as the warm serial baseline.
Its exact p50 is 63.995 ms, mean 70.587 ms, and range 53.811–113.326 ms;
all 100 creates reached guest readiness. It used the same cluster, template,
pool shape, SDK version, timeout, and 10 ms readiness criterion. Its client
Pod had a two-CPU limit with placement not explicitly pinned; the later
comparison client has a four-CPU limit and runs on one selected node. RustFS
persistence also changed between runs, although these warm creates used
cached template artifacts.

The 77.9 ms repeat remains visible because it was collected alongside the
concurrent runs with their client configuration. Both serial batches are in
the raw report and are not pooled. Their difference is not a controlled
experiment that isolates a host, client-CPU, or storage change.

Runtime logs from the same creation windows give the following breakdown.
These include each run's two untimed warmups, so their sample counts differ
from the SDK table. They stop at runtime creation, before SDK readiness:

| Load | Runtime create p50 | Rootfs preparation p50 | VM state load/resume p50 | Runtime log samples |
|---|---:|---:|---:|---:|
| Serial repeat | 50.0 ms | 2.31 ms | 0.233 ms | 102 |
| Concurrency 10 | 34.1 ms | 2.76 ms | 0.234 ms | 102 |
| Concurrency 20 | 33.5 ms | 2.63 ms | 0.225 ms | 53 |

The initial serial validation's separate runtime window has a 39.43 ms
runtime-create median and 2.286 ms rootfs-preparation median across 103
creates (100 timed, two warmups, and one subsequent lifecycle create).

::: warning Saturation result
The concurrency-20 latency distribution contains **successful attempts only**.
There were 48 `SandboxRateLimitException` failures (HTTP 429 / Fastlet
`CapacityRejected`) and one readiness timeout with repeated gateway HTTP 503.
Four test CRs also stuck in deletion with an assignment annotation/status
projection conflict. Their stale placement projections were cleared to let
the controller perform cleanup before the next experiment. This run does not
establish reliable 20-way admission or a sustainable requests-per-second rate.
The implementation cause of the admission/projection failure needs a separate
fix; replacing the host disk alone does not resolve it.
:::

## Request latency

The SDK samples use one warm sandbox and 100 sequential calls per path:

| Path | Success | p50 | p90 | p99 | Mean |
|---|---:|---:|---:|---:|---:|
| Warm SDK `/ping` through the gateway | 100/100 | 0.92 ms | 1.15 ms | 5.91 ms | 1.07 ms |
| SDK `commands.run('echo ack-perf')`, through completion and stream EOF | 100/100 | 202.8 ms | 204.3 ms | 207.1 ms | 203.2 ms |

The
[execd command handler](https://github.com/opensandbox-group/OpenSandbox/blob/c7dc78a4090e5de2b9119e9bd93952cae24f87bd/components/execd/pkg/web/controller/command.go)
has a default 200 ms stream-close grace period; the observed approximately
203 ms is consistent with that behavior. It is not a measurement of shell
process startup alone.

## Live snapshot

One live snapshot was measured:

| Phase | Measured |
|---|---:|
| Snapshot request accepted | 14.4 ms |
| Request → available snapshot (`Ready` in the SDK) | 60.63 s |
| VM frozen during dump | 11.14 s |
| Runtime artifact publication | 46.22 s |
| Artifact set | 2 GiB rootfs + 2 GiB memory |

The SDK source was healthy afterward, with its file marker and boot ID
preserved. Requests were not sampled throughout its freeze, so this establishes
no zero-interruption claim. The initial probe used an incorrect terminal-state
check and is excluded from the timings.

## Restore from snapshot

Creating a new sandbox from a published live snapshot was not measured in
this ACK run. Cached checkpoint restoration was measured as part of
[same-node pause/resume](#pause-resume); template restoration is covered by
[Create](#create). These operations have separate completion boundaries.

## Pause / resume

For this test only, the idle pool was restricted to one existing node, retaining
five Fastlets and four slots per Fastlet. Every sample's source and destination
Kubernetes node were verified through the CR's placement and the Fastlet Pod's
`spec.nodeName`. All three resumes moved to a different Fastlet on that **same
node** and used the node's checkpoint cache. The original pool selector was
restored afterward.

| Measurement | Median | Observed range (3 samples) |
|---|---:|---:|
| SDK pause request accepted | 22.4 ms | 22.0–25.5 ms |
| Pause request → durable `Paused`, including upload and release | 61.14 s | 61.13–61.23 s |
| VM frozen during checkpoint dump | 11.15 s | 11.13–11.18 s |
| Publish approximately 4 GiB (2 GiB rootfs + 2 GiB memory) | 45.61 s | 45.61–45.63 s |
| Resume runtime creation, after cached artifacts are available | 35.49 ms | 33.64–35.50 ms |
| Resume rootfs preparation | 2.92 ms | 2.64–3.26 ms |
| SDK same-node resume → guest `/ping` 200 | **62.66 s** | **58.29–65.16 s** |

All three samples preserved the filesystem marker, tmpfs marker, boot ID,
background PID, and process start time. The sandbox identity also stayed the
same. No checkpoint artifact-delivery attempt appears in the corresponding
Fastlet logs. Rootfs checkpoint cloning took 0.88–0.94 ms with zero full-copy
time. Three samples establish this observed range, not a reliable p99.

::: warning Same-node resume remains slow
Cached runtime restoration is tens of milliseconds, but usable SDK resume is
approximately a minute in this deployment. Runtime logs publish the route
immediately after restoration, leaving most elapsed time in endpoint/guest
availability and client waiting. These measurements do not isolate which
network, gateway, endpoint-resolution, or retry mechanism causes that gap.
The gap cannot be explained by copying a 2 GiB rootfs or fetching a checkpoint
onto another node. Do not substitute the runtime number for SDK resume.

The probe used a four-minute resume budget and ten-second HTTP request timeout;
the SDK's default resume budget is 30 seconds. The reported result includes
actual readiness waiting and does not skip health checks.
:::

## Artifact delivery (DART P2P)

Not verified in this ACK run. Template artifacts were already cached on both
nodes, and same-node resumes used the checkpoint cache. Cold delivery latency,
origin/peer block counts, and P2P efficiency require separate measurements.

## Boot characteristics

Cold kernel boot was not verified in this ACK run. Template creates restore
`vmstate.snap` instead of booting a kernel; their measured VM state load/resume
phases appear under [Create](#create).

## Interpreting the results

XFS reflink support and GNU `cp` keep warm rootfs preparation in the
millisecond range: the initial serial runtime window measured a 2.286 ms
median. This measures the storage/copy phase, not total SDK readiness.

Concurrency changes admission contention; durable pause includes dumping,
hashing, and uploading a checkpoint; SDK readiness includes endpoint resolution,
HTTP timeouts, and retries. The saturation failures under [Create](#create) are
control-plane evidence, not a host-only explanation. The same-node resume gap
also remains unresolved. A warm `/ping`, completed command stream, runtime
restore, and SDK resume have different completion boundaries.

## Reproducing

### SDK measurements

Use a source-built SDK matching the recorded version. Set `DOMAIN`, `API_KEY`,
and `TEMPLATE_ID` for the target cluster without putting credentials in the
report. Run from an in-cluster client to retain this network boundary:

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

`REPORT_PATH` saves raw attempts, settings, success/failure counts, and
nearest-rank percentiles. A run with failed creates returns a nonzero exit
status. Save the deployed image digests, host/storage details, template
digest, pool shape, and timestamped Fastlet/controller logs alongside it.
The SDK run used an equivalent probe that also collected lifecycle
and request samples; the report retains those original samples.

For the other measurements, retain the same SDK connection settings:

1. On one warm sandbox, time 100 sequential `sandbox.is_healthy()` calls,
   asserting success; then time 100 `sandbox.commands.run('echo ack-perf')`
   calls through stream EOF, asserting the expected output.
2. On an idle pool restricted to one node, create a fresh sandbox for each of
   three lifecycle samples. Before pausing, wait for CR
   `status.runtime.state == Ready`; the SDK's early readiness result can precede
   that projection. Write markers to the filesystem and tmpfs and record the
   boot ID and a background process's PID/start time.
3. Start the pause timer before `sandbox.pause()`. Poll
   `manager.get_sandbox_info(id).status.state == 'Paused'` every 200 ms; an
   accepted pause response alone is not a durable checkpoint.
4. Time `Sandbox.resume(id, resume_timeout=timedelta(minutes=4),
   health_check_polling_interval=timedelta(milliseconds=10),
   connection_config=config)` through its normal readiness check. Verify
   placement on the original node and all guest markers before killing the
   sandbox. Restore the pool's original selector and wait for five warm,
   ready Fastlets.
5. For live snapshot, time `sandbox.create_snapshot()` acceptance separately
   from `manager.get_snapshot(id).status.state == 'Ready'` (200 ms polling).
   The public snapshot API uses `Ready`; template builds use `Succeeded`.
   Verify the source and delete the test snapshot and sandbox.

Capture runtime dump, publish, restore, and route-publication log timestamps
to distinguish checkpoint I/O from later SDK readiness. Clean up only the
artifacts created by the test after their sandbox/snapshot resources are gone.

Keep hardware, kernel, Firecracker version, template digest, pool shape, client
placement, and artifact-store placement fixed when comparing load shapes.
