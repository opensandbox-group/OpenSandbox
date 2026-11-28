# OpenSandbox `release-1.3.0-rc.4` ACK OverlayBD Benchmark Baseline

This directory archives the full 7-suite benchmark baseline executed on 2026-11-28 against pure `release-1.3.0-rc.4` (`opensandbox` commit `8d26739750e7f14b3da1c6af17703165e0166881`) on a two-node ACK bare-metal cluster using the Firecracker + OverlayBD runtime: the initial 6 suites were collected with harness commit `cef9ae3bc`, and `rootfs-only-restore` was collected with `main` harness commit `4cecd124a` (post-#3183) and merged into the same report. Companion artifacts: [`report.json`](./report.json) (standard `schema_version: 2`, verified across all 7 suites with `opensandbox-bench compare`), [`plot.html`](./plot.html), and [`compare-rc3.html`](./compare-rc3.html) (`release-1.3.0-rc.3` vs `release-1.3.0-rc.4`).

## Methodology & Caveats Card

| Dimension | Configuration / Disclosure |
|---|---|
| Release purity | **100% pure `release-1.3.0-rc.4`** across all 10 deployed components (zero image substitutions; `fsb-fastlet:release-1.3.0-rc.4` runs natively on Lifsea 3) |
| Cluster & hardware | 2× ACK bare-metal nodes (`ecs.ebmg9ae.48xlarge`), 192 vCPU / 742.4 GiB allocatable RAM (~755 GiB physical capacity) per node, Lifsea (ContainerOS) 3, kernel `5.10.134-19.6.1.lifsea8.x86_64`, Kubernetes `v1.36.2`, `containerd://2.1.9`, `ublks_max=65535` |
| Guest network CIDR | Controller-owned `FAST_SANDBOX_NETWORK_CIDR=172.30.0.0/20` (4,094 IPs) configured on `deploy/fast-sandbox-controller` and propagated to both `sandboxtemplate-builder` and `SandboxPool` Fastlets (`fsb0=172.30.0.1/20`) |
| Template & local registry | In-cluster local `registry:2` (`opensandbox-bench-registry.opensandbox-bench-run.svc:5000`, `plainHTTP: true`), OverlayBD `Full` template (`python:3.12-bookworm` + `execd:release-1.3.0-rc.4`, `2 vCPU / 2048 MiB / 2 GiB rootfs`) |
| Prewarm & cache settings | OverlayBD `OVERLAYBD_POOL_LOW=64`, `OVERLAYBD_POOL_HIGH=80`, `OVERLAYBD_POOL_SIZE_GB=2`, `OVERLAYBD_CACHE_SIZE_GB=32`; Fastlet `FAST_SANDBOX_OCI_BLOB_CACHE_SIZE_BYTES=16 GiB`, `FAST_SANDBOX_SNAPSHOT_SPILL_DIR=/dev/shm/fsb-spill`; DART P2P disabled; client `WarmupRuns=2`, `readinessPollMs=10` |
| Harness & telemetry (`cef9ae3bc` / `4cecd124a`) | Zero RBAC 403 warnings (`telemetry_status: "ok"`), real `host.cpu_cores` (`1.28 / 4.53 / 5.44 cores`), monotonic per-request `StageAttribution` (0 `P99 < P50` paradoxes), separated `fastlet_rss_mib` vs `proxy_rss_mib`, `O_DIRECT` storage microbench (`io_path: "direct_io"`, `storage_layer: "upper_rw"`), and create latency tables focused strictly on `t_api_ack` and `t_first_ping`. Top-level `provenance` (`cef9ae3bc`) and `leak_audit` in [`report.json`](./report.json) belong to the 6-suite run; `suites["rootfs-only-restore"].extra` records its `main` harness commit (`4cecd124a47df4c3250dd9ebfa04677e36e020a8`), `run_id` (`bench-20261128-102459-cycle-2`), and dedicated passing `leak_audit` |

## Deployed Component Images (`release-1.3.0-rc.4`)

| Component | Image | Digest |
|---|---|---|
| `opensandbox-server` | `docker.io/opensandbox/server:release-1.3.0-rc.4` | `sha256:5d879226fcbb425fd329151be107368474a1b96be6d6344c918444f243d7edaf` |
| `opensandbox-controller-manager` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller:release-1.3.0-rc.4` | `sha256:3e415cfbd0fb7818568972784d7f2046386c1ae76e11705791e957d1cf434d80` |
| `opensandbox-ingress-gateway` | `docker.io/opensandbox/ingress:release-1.3.0-rc.4` | `sha256:40d425d272936900ca735fa797bfa8764799e9f1f990224fba93ff50418c5826` |
| `fast-sandbox-controller` | `docker.io/opensandbox/fsb-controller:release-1.3.0-rc.4` | `sha256:2bb61027cab13276333e13ddcf5851c4c38f7479855de9c3147f07e851f1d4c8` |
| `firecracker-agent` & `overlaybd-ublkd` | `docker.io/opensandbox/fsb-firecracker-runtime:release-1.3.0-rc.4` | `sha256:b60ca43056d192f80088b9dc9ddbe293340491184c08457cceb4334d819b703d` |
| `fsb-janitor` | `docker.io/opensandbox/fsb-janitor:release-1.3.0-rc.4` | `sha256:0ba78dec989e7d2685c68bb47f8333d2c8c07e6a3df915757a24be7363ce27fd` |
| `fsb-fastlet` | `docker.io/opensandbox/fsb-fastlet:release-1.3.0-rc.4` | `sha256:df67adb6a3cff7f2129c5815b0c5f00268a883a40ced49b1a5c74a99ff163ffd` |
| `fsb-fastlet-proxy` | `docker.io/opensandbox/fsb-fastlet-proxy:release-1.3.0-rc.4` | `sha256:48212b76b95f2363a5530a51167c50449fa3a2e9ad880705abb01307ab3e6fca` |
| `egress` | `docker.io/opensandbox/egress:release-1.3.0-rc.4` | `sha256:0afbd6f886aec8db265de58e6bd46fc8ce4183efd0703eac604306bfb78c338e` |
| `execd` | `docker.io/opensandbox/execd:release-1.3.0-rc.4` | Built into OverlayBD `Full` template |

## `release-1.3.0-rc.3` vs `release-1.3.0-rc.4` Highlights

See [`compare-rc3.html`](./compare-rc3.html) for the interactive visual comparison.

| Suite / Metric | `release-1.3.0-rc.3` | `release-1.3.0-rc.4` (`cef9ae3bc`) | Delta |
|---|---:|---:|---:|
| `schedule-burst` `t_api_ack` P50 / P95 / P99 (ms) | 294.3 / 5941.5 / 6762.5 | **74.4 / 278.0 / 370.3** | -74.7% / -95.3% / -94.5% |
| `schedule-burst` `t_first_ping` P50 / P99 (ms) | 308.1 / 6770.1 | **87.5 / 391.0** | -71.6% / -94.2% |
| `steady-churn` `t_api_ack` P50 / P95 / P99 (ms) | 191.3 / 762.7 / 1350.2 | **44.4 / 70.5 / 93.8** | -76.8% / -90.8% / -93.1% |
| `high-density-soak` `0.50x` / `1.00x` live sandboxes | 371 / 212 | **371 (0 denials) / 413 (0 OOM)** | +94.8% peak live |
| `density-ramp` single-Fastlet peak live sandboxes | 230 (`/24` limit) | **800 (`960/960` PASS, `/ping` P99 `4.24 ms`)** | 3.5× density |
| `noisy-neighbor` same-node P99 degradation ratio | 0.965× (9 victims) | **0.908× (9 victims, 0% fail)** | Stable |

## Bundle 1: Lifecycle (`schedule-burst` + `steady-churn`, 2 Fastlets × 120 Slots)

### `schedule-burst` (`559 / 768` succeeded, `72.8%`, `8.52 RPS`, overall `t_api_ack` P50/P95/P99 `74.4 / 278.0 / 370.3 ms`, `t_first_ping` `87.5 / 287.0 / 391.0 ms`)

| Concurrency `c` | N (OK / Fail) | Success % | Pending Fallback % | HTTP 429 | Throughput (RPS) | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P95 / P99 (ms) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 128 / 0 | 100.0% | 0.0% | 0 | 3.05 | 65.3 / 80.0 / 87.0 | 76.0 / 91.4 / 135.9 |
| 4 | 128 / 0 | 100.0% | 0.0% | 0 | 11.59 | 65.2 / 102.7 / 106.7 | 75.6 / 112.9 / 117.1 |
| 16 | 128 / 0 | 100.0% | 0.0% | 0 | 45.49 | 70.9 / 118.5 / 134.6 | 81.6 / 129.2 / 145.1 |
| 32 | 76 / 52 | 59.4% | 0.0% | 52 | 9.97 | 155.7 / 282.1 / 504.7 | 166.9 / 291.7 / 515.7 |
| 64 | 99 / 29 | 77.3% | 0.0% | 29 | 82.68 | 202.5 / 370.3 / 374.8 | 212.4 / 390.9 / 391.8 |
| 128 | 0 / 128 | 0.0% | 0.0% | 128 | 0.00 | 0.0 / 0.0 / 0.0 | 0.0 / 0.0 / 0.0 |

### `steady-churn` (`10 RPS × 120 s`, `427 / 1200` succeeded, `35.6%`, `2.58 RPS`)

| N (OK / Fail) | Success % | Errors | Achieved RPS | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P95 / P99 (ms) |
|---:|---:|---|---:|---:|---:|
| 427 / 773 | 35.6% | `772 × http_429`, `1 × timeout` | 2.58 | 44.4 / 70.5 / 93.8 | 54.7 / 80.9 / 103.8 |

**Observed upstream defect (`fast-sandbox#289`)**: Under sustained high-concurrency create/delete churn in Bundle 1, `fsb-fastlet:release-1.3.0-rc.4` still exhibited the `cleanSlots` leak, causing `HTTP 429` rejections. Host CPU during Bundle 1 measured `1.28 / 4.53 / 5.44 cores` (`0.67% / 2.36% / 2.83%` of 192 vCPU).

## Bundle 2: `high-density-soak` (2 Fastlets × 400 Slots, Real Node Memory, `egressProbeUrl: "none"`)

| Oversell Ratio | Active Sandboxes | Admission Denials | OOM Kills | Firecracker PSS (MiB/sbx) | Firecracker RSS (MiB/sbx) | P99 Exec Latency (ms) | Drain Recovery (s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| `0.50x` | 371 | 0 | 0 | 10.77 | 74.56 | 224.2 | 9.55 |
| `1.00x` | 413 | 329 | 0 | 10.97 | 76.56 | 218.1 | 61.00 |

## Bundle 3: Single-Fastlet `density-ramp` (`960 / 960` PASS, `placement_scope: "single_fastlet"`, `maxSandboxesPerPod=900`)

Overall `t_api_ack` P50/P95/P99 `477.5 / 866.4 / 3127.2 ms`, `t_first_ping` `484.3 / 875.5 / 3136.8 ms`, `density_ping` `2.4 / 5.3 / 6.8 ms`, zero failures across all 8 steps up to **800 concurrent live Firecracker MicroVMs on a single Fastlet**, with cleanly separated `fastlet` (`55.1 → 75.0 MiB`) and `fastlet-proxy` (`31.0 → 54.6 MiB`) RSS.

| Target Live | Delta | Batch (OK / Fail) | Step Dur (s) | `/ping` OK | `/ping` P99 (ms) | Host `ΔMemAvail` (MiB/sbx) | FC PSS (MiB/sbx) | FC RSS (MiB/sbx) | `ΔSlab` (MiB/sbx) | `fastlet` / `proxy` RSS (MiB) | Host `load_1m` |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|
| 50 | +50 | 50 / 0 | 0.55 | 20 / 20 | 7.39 | 14.93 | 10.43 | 65.09 | 1.50 | 55.1 / 31.0 | 2.08 |
| 100 | +50 | 50 / 0 | 0.52 | 20 / 20 | 3.75 | 16.10 | 10.46 | 71.90 | 2.00 | 57.3 / 33.8 | 1.84 |
| 150 | +50 | 50 / 0 | 0.59 | 20 / 20 | 4.06 | 16.50 | 10.31 | 72.28 | 2.20 | 58.7 / 35.6 | 1.67 |
| 200 | +50 | 50 / 0 | 0.67 | 20 / 20 | 3.66 | 16.58 | 10.30 | 73.41 | 2.34 | 60.5 / 38.1 | 1.52 |
| 230 | +30 | 30 / 0 | 0.53 | 20 / 20 | 4.64 | 17.02 | 10.35 | 73.86 | 2.41 | 65.8 / 41.0 | 1.44 |
| 400 | +170 | 170 / 0 | 2.34 | 20 / 20 | 3.53 | 16.86 | 10.08 | 72.57 | 2.49 | 68.8 / 46.1 | 1.30 |
| 600 | +200 | 200 / 0 | 3.64 | 20 / 20 | 3.00 | 17.08 | 10.08 | 73.69 | 2.55 | 71.0 / 46.2 | 1.80 |
| 800 | +200 | 200 / 0 | 7.17 | 20 / 20 | 4.24 | 17.50 | 10.15 | 74.51 | 2.61 | 75.0 / 54.6 | 2.66 |

## Bundle 4: `dataplane-microbench` & `noisy-neighbor`

- **`dataplane-microbench`** (`11 / 11` PASS, `measurement_scope: "guest_absolute"`, `11.23 s`, `networkTargetUrl: "none"`): CPU (`python3-fallback`) single `2199.1 events/s`, 4-way concurrent mean `2217.9 events/s` (`scaling_efficiency_ratio = 1.009x`); Storage (`python3-dd-fallback`, 64 MiB, `io_path: "direct_io"`, `storage_layer: "upper_rw"`) 4K rand read `866520 IOPS`, 4K rand write `702896 IOPS`, 1M seq cold read `7663.8 MiB/s`, 1M seq hot read `19613.4 MiB/s`, 1M seq write `908.5 MiB/s`; Network explicitly skipped via `networkTargetUrl: "none"`.
- **`noisy-neighbor`** (`191 / 191` PASS, `5.98 s`, single-Fastlet 100% same-node co-located `colocated_victim_count=9`, 1 aggressor [10%] + 9 victims [90%], `cpu+disk`): Baseline victim exec P50/P95/P99 `209.0 / 220.1 / 231.4 ms` vs Under-stress `208.4 / 209.6 / 210.1 ms`, **`same_node_p99_degradation_ratio = 0.908x`** (0% failure).

## Bundle 5: `rootfs-only-restore` (`37 / 37` PASS, `main` harness `4cecd124a`, dual-node)

Collected on the same pure `release-1.3.0-rc.4` two-node cluster (`run_id: bench-20261128-102459-cycle-2`, duration `99.48 s`, `telemetry_status: "ok"`, `t_api_ack` P50/P95/P99 `101.5 / 180.0 / 234.3 ms`, `t_first_ping` `907.4 / 1489.2 / 1586.6 ms`, `guest_boot_to_first_ping_gap` P50 `805.1 ms`). All-node OverlayBD cache eviction (`cold_cache_evicted: true`) ran cleanly across all three dirty-upper tiers (`0 / 16 / 256 MiB`) after warm and fan-out restores completed teardown.

### Dirty-Upper Ladder (Capture, Source Stall & Warm vs Cold Cache Restore)

| Dirty Upper (MiB) | Snapshot Create (ms) | Source Stall (ms) | Post-Capture OK | Warm First Ping / Boot Gap (ms) | Cold First Ping / Boot Gap (ms) | Cold Cache Evicted | Markers (Warm/Cold) |
|---:|---:|---:|---|---:|---:|---|---|
| 0 | 1030.1 | 42.7 | true | 886.4 / 824.0 | 1489.2 / 1389.5 | true | true / true |
| 16 | 1036.2 | 42.3 | true | 894.1 / 828.0 | 1586.6 / 1352.2 | true | true / true |
| 256 | 2045.4 | 342.3 | true | 884.6 / 813.6 | 1471.0 / 1291.0 | true | true / true |

### Concurrent Fan-Out (Warm Cache, 0 MiB Tier)

| Concurrency | OK / Total | `t_first_ping` P50 / P95 / P99 (ms) | `guest_boot_gap` P50 / P95 / P99 (ms) |
|---:|---:|---|---|
| 1 | 1 / 1 | 879.1 / 879.1 / 879.1 | 801.0 / 801.0 / 801.0 |
| 8 | 8 / 8 | 894.5 / 1462.8 / 1462.8 | 804.2 / 1355.0 / 1355.0 |
| 16 | 16 / 16 | 907.6 / 921.6 / 921.6 | 795.2 / 811.7 / 811.7 |

### Cross-Node Cold Boot & Resource Overrides

- **Cross-Node Cold Boot**: **PASS** (`ap-southeast-1.10.79.205.203` -> `ap-southeast-1.10.79.205.204`), First Ping `883.4 ms` (`guest_boot_gap: 815.1 ms`), Marker Verified `true`.
- **Resource Overrides** (base template `2 vCPU / 2048 MiB`):

| Target (vCPU / MiB) | Observed (vCPU / MiB) | Entrypoint | First Ping / Boot Gap (ms) | Override Verified | Status |
|---|---|---|---:|---|---|
| 1 / 1024 | 1 / 984.7 | - | 912.3 / 827.2 | true | PASS |
| 2 / 1024 | 2 / 984.5 | - | 879.9 / 805.1 | true | PASS |

## Leak & Stability Audit

The top-level `leak_audit` in [`report.json`](./report.json) reflects the 6-suite run (`cef9ae3bc`, failed on 1 dynamic non-idle ublk device); the separate `rootfs-only-restore` run (`4cecd124a`, recorded in `suites["rootfs-only-restore"].extra.leak_audit`) passed all 8 checks (`0` OOM, `0` residual CRs, `0` stuck deleting slots, `0` orphaned ublk devices, `0` leaked Firecracker PIDs, `13.92 GiB` StateRoot free headroom).

| Check | Status (6-suite run) | Status (`rootfs-only-restore`) | Notes |
|---|---|---|---|
| Host / Pod cgroup OOM | `pass` (0 / 0) | `pass` (0 / 0) | Zero OOM kills across all 7 suites |
| Residual Sandbox / Snapshot CRs & Firecracker PIDs | `pass` (0 / 0) | `pass` (0 / 0) | Adaptive `70 s` grace triggered on 800-sandbox teardown; full async cleanup converged cleanly to 0 |
| `cleanSlots` Conservation (Bundle 1) | **Known Defect (`fast-sandbox#289`)** | `pass` | Reproduced under high-concurrency create/delete churn in Bundle 1; Fastlets were recreated cleanly before Bundles 2–5 |
| `ublk_devices` | `1` (1-node) / `2` (2-node) | `pass` (`0` orphaned) | Dynamic warm-pool expansion added 1 non-idle `/dev/ublkb*` device per node in the 6-suite run (`0` leaked Firecracker PIDs / `0` residual CRs) |

## Scope & Known Limitations

This baseline evaluates lifecycle scheduling, OverlayBD warm-pool caching, high-density memory scaling up to 800 MicroVMs on one Fastlet, guest-absolute CPU and `direct_io` upper-layer storage throughput, same-node noisy-neighbor isolation, and RootfsOnly snapshot/cold-boot restore (warm vs evicted cold cache, fan-out, cross-node, and resource scale-down overrides). Bare-metal host-control ratios for `dataplane-microbench`, Pause/Resume, and Wake-Storm matrix runs remain outside the scope of this archive. Note that `rootfs-only-restore` was executed via `opensandbox-bench run` (`worker run`), which populates Kubernetes node metadata without re-running host provisioner preflight probes (both ACK nodes were independently verified with KVM, `ublk_drv`, XFS reflink, and `/dev/shm/fsb-spill`).
