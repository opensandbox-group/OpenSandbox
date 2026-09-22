---
title: Fast Sandbox Performance
description: Measured performance characteristics of the fast-sandbox integration — create hot path, snapshot restore, live snapshot, pause/resume, request latency, and artifact delivery.
---

# Performance

This page collects performance figures measured on real hardware: runtime paths come from the Firecracker integration verification harness (`integration-env.sh verify-all`), and the create hot path additionally includes a sustained concurrent-create benchmark through the full OpenSandbox stack. Figures are reported as the tools emitted them — single-run observations, not tuned benchmark medians.

Performance here is a consequence of the architecture, not an optimization pass: creates are constant-time admissions into pre-warmed capacity (see [Scheduling](/architecture/fast-sandbox/scheduling)), warm creates restore from a snapshot instead of booting a kernel (see [Firecracker](/architecture/fast-sandbox/firecracker)), and pause/resume rides the submit-and-converge mutation path (see [High Availability](/architecture/fast-sandbox/ha)).

## How to read these numbers

- **Pre-baked template path**: the figures below describe creates and restores where the template's artifacts are already in the node cache (pre-warmed Fastlets, snapshot-backed). First use of a template or snapshot on a node pays a one-time artifact delivery (~3.5 GiB through DART) and is not the subject of this page.
- **Fastlet-side vs end-to-end**: restore-phase figures come from the runtime agent's own timing logs; end-to-end figures include the client harness, lifecycle server, and FastPath.
- These are functional-verification measurements. They demonstrate the order of magnitude of each path; they are not a throughput benchmark.

## Environment

All figures were measured on a single physical host running the entire integration environment — both kind nodes (control-plane + worker) and the MinIO artifact store are colocated on this machine, so "cross-host" resume means a move between kind nodes on the same physical host:

- **Host**: Alibaba Cloud Linux 3, kernel 5.10.134; Intel Xeon Platinum 8163 @ 2.50 GHz (48 cores / 96 threads, 2 NUMA nodes); 503 GiB RAM
- **Virtualization**: `/dev/kvm` and `/dev/net/tun` passed through, VMX on all 96 threads; cgroup v2
- **State root**: XFS with `reflink=1` (60 GiB loop device mounted at `/var/lib/fast-sandbox`, backed by NVMe ext4)
- **Cluster**: kind v0.24.0 on docker 24.0.0, 2 nodes with KVM passthrough
- **Storage**: MinIO (`minio/minio:latest`) container as the S-compatible artifact store, on the same host
- **Workload**: `alpine:3.19` golden-image template with `execd` 1.1.0; Firecracker v1.16.1 (integration default)

## Create

| Scenario | End-to-end (run → first `/ping` 200) | Fastlet-side restore |
|---|---|---|
| Create from a pre-baked template (snapshot + artifacts in node cache) | 115 ms (run RPC 59 ms + restore 35 ms) | 35 ms |
| Burst: 5 concurrent creates across 2 Fastlets (10 slots) | run RPC 60–71 ms each; first 200 in 52–55 ms | ~36 ms |
| Teardown: 7 sandboxes deleted | leases drained + jail dirs cleaned in 0.5 s | — |

Notes:

- Restore phases: acquire 0.15 ms, rootfs reflink 0.7 ms, launch 20.7 ms, configure 2.8 ms, vmstate boot 0.3 ms.
- Burst growth in total time comes from the harness's sequential probe loop (queue-to-probe up to 2.1 s), not from platform admission: every create RPC returned in 60–71 ms and every first probe hit 200 in ~53 ms.

### Sustained concurrent creates (full OpenSandbox stack)

Measured through the full OpenSandbox stack — Python SDK create → lifecycle server → FastPath → fast-sandbox, on the same reference host (see [Environment](#environment) and [Reproducing](#reproducing)). Method: 100 creates issued as batches of 10 concurrent creates across 2 Fastlet pods (20 slots), 5 s between batches for slot reclamation, template artifacts already in the node cache. 0 of 100 creates failed.

| Metric | Measured |
|---|---|
| End-to-end create → Ready, 100 runs | p50 235 ms · p90 285 ms · p99 325 ms (avg 236 ms) |
| Range | min 138 ms – max 330 ms |
| Reference: sequential warm create (batch warmup) | 80–100 ms |

Even at 10-way concurrency, p99 stays under 0.35 s with zero failures. The gap between the ~115 ms single-create figure above and the ~235 ms p50 is concurrency overhead — 10 concurrent restores sharing the Fastlet slots plus the lifecycle-server and SDK round trips — not a change in the restore path itself, which stays a ~35 ms snapshot resume.

## Request latency

| Path | Cold | Warm |
|---|---|---|
| `/ping` through the proxy chain (client → fastlet-proxy → execd) | 9 ms | 8 ms |
| execd `/ping` API round trip | — | ~4 ms |

## Live snapshot

Measured on the live-snapshot verification run. The source sandbox kept serving throughout — the guest is only frozen for the pause window:

| Phase | Measured |
|---|---|
| CreateSnapshot RPC | 33 ms |
| Creating → Publishing (dump, spill, rootfs clone, staging) | 16.0 s |
| Publishing → Succeeded (artifact upload) | 7.5 s (driver publish API 8.3 s for 3.5 GiB) |
| Total create → Succeeded | 23.6 s |
| VM pause window (guest frozen) | 232 ms (dump API 231 ms) |
| Guest-visible impact during snapshot | 0 ping failures, max gap 0 ms |
| Artifact set | rootfs.ext4 3.0 GiB, memory.snap 512 MiB, vmstate.snap 11 KiB |

The guest's monotonic state is untouched by the snapshot: source uptime continued 6.1 s → 33.0 s across the snapshot, with `/ping` and in-guest execution healthy before and after.

## Restore from snapshot

| Scenario | Measured |
|---|---|
| Restore with artifacts already local (runtime phase) | 205 ms — rootfs reflink 171 ms + launch 20.5 ms + configure 2.7 ms + vmstate boot 0.3 ms |

First use of a published snapshot on a node additionally pays the one-time ~3.5 GiB store pull (~20 s in this environment) before the restore.

The manifest's recorded egress policy is re-applied automatically on restore; an explicit create-time binding overrides it (verified in the same run).

## Pause / resume

| Phase | Measured |
|---|---|
| PauseSandbox → Paused (checkpoint durable, runtime released) | 24.3 s |
| — VM frozen window during checkpoint | 257 ms (dump API 256 ms, spill move 391 ms) |
| — publish checkpoint to artifact store | 7.9 s (3.5 GiB) |
| Cross-host resume (node cache dropped, Fastlet replaced) → Ready | 20.7 s — incl. 16.7 s checkpoint pull from the store; the restore itself is 299 ms |
| Local resume (checkpoint in node cache) → Ready | 1.0 s, 0 store pulls |
| Guest memory state | preserved: uptime monotonic (6.2 s → 75.0 s across pause/resume), in-guest marker survived — memory restored, not rebooted |

## Artifact delivery (DART P2P)

| Observation | Measured |
|---|---|
| Cold cluster delivery of the ~3.5 GiB template (2 nodes) | 897/897 blocks from the origin — ~1 origin fetch per block cluster-wide; 192 blocks from peers |
| Warm block reads | origin delta = 0 on both nodes — served from the node block cache and peers |

## Boot characteristics

The pre-baked template path never boots a kernel: the VM resumes from `vmstate.snap`, and the restore path's "boot" phase (vmstate load + resume) is ~0.3 ms — this is what makes template creates ~100 ms. For completeness, a cold VM boot from a golden image without a snapshot takes ~1.6 s from `InstanceStart` to first response (kernel boot ~1.0 s) — see [Firecracker: Measured characteristics](/architecture/fast-sandbox/firecracker#measured-characteristics).

## Reproducing

The figures come from the fast-sandbox integration harness. To reproduce:

1. **Host**: bare-metal Linux with KVM passthrough (`/dev/kvm`, `/dev/net/tun`), Docker, Go ≥ 1.25, cgroup v2, and `sudo` for the XFS loop mount (the reference host is described in [Environment](#environment)).
2. **Source**: clone [fast-sandbox](https://github.com/opensandbox-group/fast-sandbox) and check out the commit pinned by this repository in `manifests/third-party/fast-sandbox.commit` (`d51fa69` at the time of writing):

   ```bash
   git clone https://github.com/opensandbox-group/fast-sandbox.git
   git -C fast-sandbox checkout d51fa6987366e52e87b718684ca7533bc5ecdf0e
   ```

3. **Environment**: `./scripts/integration-env.sh up` builds the images and brings up the two-node kind cluster (KVM passthrough), the MinIO artifact store, the SandboxTemplate golden image, and the pool.
4. **Figures**: `./scripts/integration-env.sh verify-all` runs the verification battery — base delivery (create timings), DART P2P evidence, execd API battery, live snapshot, cross-host pause/resume, egress matrix — and prints the timings shown on this page. Evidence logs land under the workspace's `logs/` directory (workspace default: `/data/fast-sandbox-env`).

To measure through the OpenSandbox layers instead, this repository ships the equivalent full-stack environment — fast-sandbox at the same pinned commit plus the source-built server, ingress gateway, and egress — as `scripts/fast-sandbox-env/integration-env.sh up`. The sustained concurrent-create figures under [Create](#create) come from this environment: create 100 sandboxes through the Python SDK as batches of 10 concurrent creates with a 5 s pause between batches, and report the create → Ready latency percentiles.

When re-measuring, pin: node hardware, kernel version, Firecracker version, template digest, pool shape, and artifact-store placement — otherwise the numbers are not comparable across runs.
