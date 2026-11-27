# OpenSandbox `release-1.3.0-rc.3` ACK OverlayBD Benchmark Report

This report archives the `release-1.3.0-rc.3` (`opensandbox` commit `ad653af12d21fedb3871fbf0b07ad2dcefb0a565`, `fast-sandbox` commit `5b96ba7f71e019bcce1c00fc24ddeb2b7a32ce74`) ACK OverlayBD baseline executed on 2026-11-27. Companion machine-readable artifacts in this directory: [`report.json`](./report.json) (standard `schema_version: 2`, verified with `opensandbox-bench compare`) and [`host-memory-profile.json`](./host-memory-profile.json) (0.5 s host and Firecracker PSS/RSS sampling during single-Fastlet `density-ramp`).

## Methodology & Caveats Card

- **Warm-path & pre-warming configuration**: Fastlets pre-initialize `cleanSlots` up to `maxSandboxesPerPod` (`120` per Fastlet in Bundles 1–2; `240` on a single Fastlet in Bundle 3); `overlaybd-ublkd` maintains an OverlayBD `ublk` warm pool on each node with a `16 GiB` (`17179869184` bytes) local OCI blob cache (`FAST_SANDBOX_OCI_BLOB_CACHE_SIZE_BYTES`) and `/dev/shm/fsb-spill` snapshot spill directory; `schedule-burst` runs warmups before timed measurements.
- **Readiness polling interval**: `readiness_poll_ms = 10` (vs SDK default `200 ms`) to keep polling quantization below 10 ms while measuring `t_first_ping`.
- **Registry boundary**: Template OCI layers are pulled over ACR VPC HTTPS rather than an in-cluster unauthenticated `registry:2` so storage caching and network paths reflect realistic managed-cluster deployment.
- **Minimal image substitution exception**: Nine of ten deployed components ran unmodified `release-1.3.0-rc.3` images; `fsb-fastlet` ran `586ca2d62d7ffae0c806433b695f0f9b6b23fc70` (`sha256:a5852cff57c545e0ed66e2b872c3914fd6a8035ae12d753896a3f87882647892`) solely for Alibaba Cloud Linux Lifsea (ContainerOS) 3 PSI compatibility.
- **Host & component telemetry**: RBAC (`ClusterRole` `opensandbox-bench-runner-reader`) was granted to `opensandbox-bench-run:default` prior to all runs in this archive; all `load_profiles` recorded **0 RBAC 403 warnings** and real host metrics (`node_memory_mib = 760,205`).

## Environment, Hardware & Deployed Images

| Dimension | Configuration |
|---|---|
| Cluster | 2 ACK nodes (`ap-southeast-1.10.79.205.203`, `ap-southeast-1.10.79.205.204`), region/zone `ap-southeast-1` / `ap-southeast-1c`, `ecs.ebmg9ae.48xlarge` |
| Compute per node | 192 vCPU (`191450m` allocatable), ~755 GiB RAM (`778450200Ki` / `778450144Ki` allocatable, `760,205 MiB` real observed node memory), 1,113 pods |
| OS / Kernel / Runtime | Alibaba Cloud Linux Lifsea (ContainerOS) 3, kernel `5.10.134-19.6.1.lifsea8.x86_64` (`amd64`), Kubernetes `v1.36.2-aliyun.1`, `containerd://2.1.9` |
| Kernel limits & bridge | `ublks_max=65535`, Fastlet bridge `fsb0` `172.30.0.1/24` |
| Sandbox template | OverlayBD Firecracker template (`2 vCPU / 2048 MiB RAM / 256 pids`, `ResourceAware`, `overcommitRatio` CPU `8` / Memory `4`, `egress` `dns+nft`) |
| Default pool (`opensandbox-bench-pool`) | 2 Fastlets (1 per node via `podAntiAffinity`), `maxSandboxesPerPod=120` (240 total slots) |
| Bundle 3 pool override (`density-ramp`) | 1 Fastlet (`poolMin=1, poolMax=1`), `maxSandboxesPerPod=240`, `batchCreateConcurrency=32` (restored to 2 Fastlets / `maxSandboxesPerPod=120` afterward) |

| Component | Image | Digest |
|---|---|---|
| `opensandbox-server` | `docker.io/opensandbox/server:release-1.3.0-rc.3` | `sha256:b97c10eaf9b5455cffe7c800fcecb264acd6856024e2ff00b9ce4beeab5239b8` |
| `opensandbox-controller-manager` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller:release-1.3.0-rc.3` | `sha256:ad0ed9ded0fa7ba6da5ddb17f3dcddb87ace4f2f70eabef4a57669eff781ba51` |
| `opensandbox-ingress-gateway` | `docker.io/opensandbox/ingress:release-1.3.0-rc.3` | `sha256:9b0471e0eecd951e3fc0c8f0d7c998ada6bba662e80de0f39602547cf8138b97` |
| `fast-sandbox-controller` | `docker.io/opensandbox/fsb-controller:release-1.3.0-rc.3` | `sha256:e2d30def4c418a93704e73ba2db2f79469bcbd97fad61954819f5ab2a615a5c3` |
| `firecracker-agent` & `overlaybd-ublkd` | `docker.io/opensandbox/fsb-firecracker-runtime:release-1.3.0-rc.3` | `sha256:782c12a0a8a54cc4c1a8f0c2d2475bcd719c5672d0d942dee2d17f4dfd1136cb` |
| `fsb-janitor` | `docker.io/opensandbox/fsb-janitor:release-1.3.0-rc.3` | `sha256:df89f3b1411d45b4ce34b3d03cbdcbc439873ee50c8609af17a21e8677d79318` |
| `fsb-fastlet` (**Lifsea 3 PSI exception**) | `docker.io/opensandbox/fsb-fastlet:586ca2d62d7ffae0c806433b695f0f9b6b23fc70` | `sha256:a5852cff57c545e0ed66e2b872c3914fd6a8035ae12d753896a3f87882647892` |
| `fsb-fastlet-proxy` | `docker.io/opensandbox/fsb-fastlet-proxy:release-1.3.0-rc.3` | `sha256:07c176db318756bd5e299dbd6d74e3ceee877a8f2a0c10f012c0e35b00a239a9` |
| `egress` | `docker.io/opensandbox/egress:release-1.3.0-rc.3` | `sha256:b1156af379311904e39bf32b8d7d24032e3e53fdde53bd015c64d3bf0f8f04d2` |
| `fsb-sandboxtemplate-builder` | `docker.io/opensandbox/fsb-sandboxtemplate-builder:release-1.3.0-rc.3` | `sha256:4664c7a5a99b396019fc9ea611133528af3d6ccb85966e231a246fd53acda318` |

## Bundle 1: Lifecycle (`schedule-burst` + `steady-churn`)

Bundle 1 ran `schedule-burst` (`c=1, 4, 16, 32, 64, 128`, 128 attempts per level, 768 total) followed by `steady-churn` (target `10 RPS` × `120 s`, 1,200 total attempts) on two Fastlets (`maxSandboxesPerPod=120`).

### `schedule-burst` concurrency gradient

Overall result: **654 / 768 succeeded** (`85.2%`, `114 × http_429`, `7.69 RPS`, `85.04 s`). The zero-error sustainable knee on this 2-Fastlet `rc.3` deployment is **`c=32`** (`128/128`, `0 × http_429`, `25.90 RPS`); overload begins at `c=64` due to the `cleanSlots` leak noted below.

| Concurrency `c` | N (ok/fail) | Success % | Pending Fallback % | HTTP 429 | Throughput (RPS) | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P95 / P99 (ms) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 128 / 0 | 100.0% | 0.0% | 0 | 2.59 | 141.4 / 186.0 / 210.2 | 152.8 / 197.8 / 221.8 |
| 4 | 128 / 0 | 100.0% | 0.0% | 0 | 8.08 | 164.9 / 376.0 / 406.2 | 175.2 / 387.8 / 418.0 |
| 16 | 128 / 0 | 100.0% | 0.0% | 0 | 20.22 | 425.7 / 805.0 / 861.3 | 440.9 / 817.6 / 870.8 |
| **32 (zero-error knee)** | **128 / 0** | **100.0%** | **0.0%** | **0** | **25.90** | **694.9 / 1167.0 / 1247.4** | **706.6 / 1179.7 / 1257.1** |
| 64 | 85 / 43 | 66.4% | 0.0% | 43 | 14.48 | 470.6 / 3936.7 / 3944.5 | 479.8 / 3945.5 / 3954.2 |
| 128 | 57 / 71 | 44.5% | 0.0% | 71 | 21.41 | 552.4 / 1154.2 / 2404.3 | 562.2 / 1161.0 / 2417.7 |

### `steady-churn` (10 RPS × 120 s)

| Suite | N (ok/fail) | Success % | Pending Fallback % | Errors | Achieved RPS | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P95 / P99 (ms) |
|---|---:|---:|---:|---|---:|---:|---:|
| `steady-churn` | 511 / 689 | 42.6% | 0.0% | `688 × http_429`, `1 × timeout` | 4.06 | 192.4 / 650.7 / 892.8 | 201.4 / 659.0 / 906.1 |

### Bundle 1 host & component load profile (0 warnings)

| Component / Metric | Avg | P95 | Max / Min |
|---|---:|---:|---:|
| Host `cpu_util_pct` (user / sys / iowait) | 0.69% (0.28 / 0.42 / 0.02%) | 2.16% (0.78 / 1.38 / 0.11%) | 2.91% (1.05 / 1.86 / 0.18%) |
| Host `load_1m` | 1.65 | 5.26 | 6.36 |
| Host `mem_available_mib` | 760,704.3 MiB | — | min 759,903.5 MiB |
| Host `page_cache_mib` / `stateroot_free_headroom_gib` | 127,141.6 MiB | — | min 9.59 GiB headroom |
| Host `firecracker_pid_count` / `open_fds` | 1.76 / 19,151 | 11 / — | max 21 / max 22,656 |
| `fastlet` CPU / RSS / `podcgroup.memory_current_mib` | 0.013 cores / 48.1 MiB / 177.3 MiB | 0.029 cores / 66.8 MiB / 368.8 MiB | 0.038 cores / 70.8 MiB / 632.3 MiB |
| `firecracker_runtime` CPU / RSS | 0.214 cores / 127.8 MiB | 0.912 cores / 135.6 MiB | 1.205 cores / 141.5 MiB |

**Known defect (`fast-sandbox#289`)**: Under high-concurrency create/delete churn (`c=64/128` and late `steady-churn`), `fsb-fastlet:586ca2d62d` leaked network slots (`capacity=120 clean=0 bound=21 destroying=1 slotCount=22`), causing `HTTP 429 NetworkUnavailable` rejections and leaving both Fastlets stuck at `clean=0` after Bundle 1. Fresh Fastlet pods were recreated before Bundle 2 and Bundle 3.

## Bundle 2: `high-density-soak` (Real 760,205 MiB Node Memory)

With RBAC fixed, `high-density-soak` sampled the real node memory (`node_memory_mib = 760,205 MiB`, `template_memory_mib = 2,048 MiB`) instead of falling back to `8,192 MiB`.

| Suite | N (ok/fail) | Success % | Pending Fallback % | Errors | Throughput (RPS) | Duration (s) |
|---|---:|---:|---:|---|---:|---:|
| `high-density-soak` | 631 / 406 | 60.8% | 0.0% | `286 × http_429`, `120 × error` | 4.28 | 147.34 |

### Density frontier (`node_memory_mib = 760,205 MiB`)

| Oversell Ratio | Active Sandboxes | Admission Denials | OOM Kills | P99 Exec Latency (ms) | Host `mem_available_min_mib` | Drain Recovery (s) |
|---|---:|---:|---:|---:|---:|---:|
| `0.50x` | **186** | **0** | **0** | **223.1** | 755,718.5 MiB | 8.04 |
| `1.00x` | 85 (target 371) | 286 | 0 | 219.6 | 756,934.3 MiB | 4.25 |

- **`1.00x` cap note**: During `1.00x` ramp (target 371), node `.204` lost its `fast-sandbox.io/firecracker-node` label, leaving a single Fastlet (`maxSandboxesPerPod=240`) combined with slot leakage under churn, which capped admission at 85 live sandboxes (`286 × http_429`) with 0 OOM kills.
- **`soak_egress_probe` root cause (`120 × error`)**: `high_density_soak.go:499` defaults `egressProbeURL` to the in-cluster Kubernetes Service DNS `http://opensandbox-server.opensandbox-system.svc:80/health`, which Firecracker guest VMs cannot resolve or dial directly, causing all 120/120 `soak_egress_probe` checks to fail while `soak_exec`, `soak_ingress`, and `soak_churn` succeeded.

### Bundle 2 host & component load profile (0 warnings)

| Component / Metric | Avg | P95 | Max / Min |
|---|---:|---:|---:|
| Host `cpu_util_pct` (user / sys / iowait) | 0.59% (0.24 / 0.35 / 0.02%) | 2.29% (0.75 / 1.65 / 0.18%) | 4.09% (1.31 / 3.34 / 0.38%) |
| Host `load_1m` | 1.06 | 3.77 | 4.47 |
| Host `firecracker_pid_count` | 59.2 | 186 | max 186 |
| Host `mem_available_mib` | — | — | min 755,645.1 / max 761,283.5 MiB (~5.64 GiB net delta at 186 live sandboxes) |
| `fastlet` `podcgroup.memory_current_mib` | — | — | max 3,728.8 MiB |

## Bundle 3: Single-Fastlet `density-ramp` (0 → 230) & Per-Sandbox Memory Overhead

Bundle 3 ramped a single Fastlet (`poolMin=1, poolMax=1, maxSandboxesPerPod=240, batchCreateConcurrency=32`, within the default `fsb0 172.30.0.1/24` subnet limit) from 0 to **230 concurrent live sandboxes** across five steps.

### Suite summary & step table

| Suite | N (ok/fail) | Success % | Pending Fallback % | Peak Live | Throughput (RPS) | Duration (s) | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P95 / P99 (ms) | `density_ping` P50 / P95 / P99 (ms) |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| `density-ramp` | **330 / 0** | **100.0%** | **0.0%** | **230** | **62.13** | 5.31 | 541.6 / 904.9 / 925.7 | 551.9 / 911.8 / 932.8 | 2.7 / 6.6 / 7.2 |

| Target Live Sandboxes | Batch Delta | N (ok/fail) | Success % | Step Duration (s) | Sampled `/ping` Healthy | Sampled `/ping` P99 (ms) |
|---:|---:|---:|---:|---:|---:|---:|
| 50 | +50 | 50 / 0 | 100.0% | 0.90 | 20 / 20 | 7.59 |
| 100 | +50 | 50 / 0 | 100.0% | 1.02 | 20 / 20 | 3.31 |
| 150 | +50 | 50 / 0 | 100.0% | 1.14 | 20 / 20 | 4.59 |
| 200 | +50 | 50 / 0 | 100.0% | 1.32 | 20 / 20 | 3.85 |
| **230** | +30 | **30 / 0** | **100.0%** | **0.93** | **20 / 20** | **4.61** |

### Bundle 3 host load profile (`load_profiles.host`, 0 warnings)

- Host `cpu_util_pct`: avg `2.55%`, P95 `4.72%`, max `4.72%` (user `0.77 / 1.28%`, sys `1.78 / 3.48%`, iowait `0.19 / 0.39%`).
- Host `load_1m`: avg `1.12`, P95 `2.99`, max `2.99`; `mem_available_mib`: min `756,878.0 MiB` / max `761,242.9 MiB`; `open_fds`: max `22,272`.

### Three-perspective per-sandbox memory overhead (0.5 s host & process sampling)

Collected on target node `ap-southeast-1.10.79.205.203` (`host-memory-profile.json`, 0-sandbox baseline: `MemAvailable = 759,117.61 MiB`, `Slab = 6,826.59 MiB`, `fastlet = 46.96 MiB`, `egress = 17.56 MiB`, `overlaybd-ublkd = 1,291.01 MiB`, `fastlet-proxy = 14.65 MiB`). Each sandbox is configured with `2 vCPU / 2048 MiB` guest RAM on OverlayBD.

| Step (Observed FC PIDs) | ① Host `ΔMemAvailable` (MiB) | ① **Host Net Delta / sbx (MiB/sbx)** | ② **Firecracker PSS Mean (MiB/sbx)** | ② **Firecracker RSS Mean (MiB/sbx)** | Kernel `Slab` Delta (MiB) | ③ `fastlet` / `egress` / `ublkd` / `proxy` RSS (MiB) |
|---|---:|---:|---:|---:|---:|---|
| Baseline (0) | 0.0 | — | 0.00 | 0.00 | 0.0 | 47.0 / 17.6 / 1291.0 / 14.7 |
| `50` (fc=50) | 476.9 | **9.54** | **7.00** | 35.76 | +30.4 | 51.3 / 22.4 / 1262.8 / 21.4 |
| `100` (fc=100) | 1,020.9 | **10.21** | **7.04** | 37.33 | +136.8 | 51.0 / 23.1 / 1127.0 / 22.2 |
| `150` (fc=150) | 1,631.8 | **10.88** | **7.00** | 37.74 | +249.6 | 63.7 / 22.9 / 977.9 / 26.1 |
| `200` (fc=208) | 2,136.6 | **10.27** | **7.07** | 37.92 | +373.2 | 68.8 / 22.8 / 894.8 / 27.8 |
| **`230` Peak (fc=230)** | **2,521.2** | **10.96** | **7.22** (`Private_Dirty=7.05`) | **38.66** | **+440.3** (`+1.91/sbx`) | **55.3 / 23.3 / 936.2 / 29.7** |
| Post-Drain (fc=0) | -739.8 (fully reclaimed) | — | 0.00 | 0.00 | -52.3 | 52.8 / 23.6 / 397.3 / 32.0 |

At 230 concurrent live sandboxes, total host memory cost is **10.96 MiB/sandbox** (driven by **7.22 MiB/sandbox** Firecracker PSS and **+1.91 MiB/sandbox** kernel slab overhead), while `fastlet`, `egress`, and `fastlet-proxy` grow by less than `15 MiB` each across the entire 230-sandbox ramp and all memory is reclaimed after drain.

## Leak & Stability Audit

`LeakAudit` verifies host OOM kills, pod cgroup OOM kills, residual `Sandbox` / `SandboxSnapshot` CRs, stuck `deleting` slots, orphaned `ublk` devices, and leaked Firecracker PIDs. It does **not** detect lost `cleanSlots` inside a Fastlet's in-memory slot pool; that defect is reported separately below.

| Bundle | `LeakAudit` Status | Host / Cgroup OOM | Residual CRs (Sbx / Snap) | Stuck Deleting Slots | Orphaned `ublk` | Leaked FC PIDs | Additional Notes |
|---|---|---:|---:|---:|---:|---:|---|
| Bundle 1 (`lifecycle`) | **PASS** (scoped) | 0 / 0 | 0 / 0 | 0 | 0 | 0 | **In-memory `cleanSlots` leak (`fast-sandbox#289`)** left both Fastlets at `clean=0` and required pod recreation before Bundle 2. |
| Bundle 2 (`high-density-soak`) | **PASS** | 0 / 0 | 0 / 0 | 0 | 0 | 0 | 186 live sandboxes drained cleanly (`8.04 s` at `0.50x`, `4.25 s` at `1.00x`). |
| Bundle 3 (`density-ramp`) | **PASS** | 0 / 0 | 0 / 0 | 0 | 0 | 0 | `cr_convergence_seconds = 10.32 s`; all 230 sandboxes and host memory fully reclaimed post-drain. |

## Scope & Known Limitations

- **Covered in this archive**: Control-plane scheduling & burst admission (`schedule-burst`), sustained create/delete churn (`steady-churn`), real-memory high-density soak (`high-density-soak`), single-Fastlet concurrent density ramp (`density-ramp` 0→230), host/component load profiling, three-perspective per-sandbox memory overhead, and post-run leak auditing.
- **Out of scope (next phase)**: In-sandbox data-plane microbenchmarks (`sysbench` CPU vs bare-metal, `fio` OverlayBD `ublk` random/sequential IOPS & throughput cold vs warm cache, `iperf3` network throughput & RTT through `egress` / `fastlet-proxy`) and multi-tenant `noisy-neighbor` isolation degradation are not included in this `rc.3` archive and are tracked for the next benchmark phase.
