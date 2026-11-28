# OpenSandbox `release-1.3.0-rc.4` ACK OverlayBD Benchmark Baseline

This directory archives the full 6-suite benchmark baseline executed on 2026-11-27 against pure `release-1.3.0-rc.4` (`opensandbox` commit `8d26739750e7f14b3da1c6af17703165e0166881`) on a two-node ACK bare-metal cluster using the Firecracker + OverlayBD runtime. Companion artifacts: [`report.json`](./report.json), [`plot.html`](./plot.html), and [`compare-rc3.html`](./compare-rc3.html) (`release-1.3.0-rc.3` vs `release-1.3.0-rc.4`).

## Methodology & Caveats Card

| Dimension | Configuration / Disclosure |
|---|---|
| Release purity | **100% pure `release-1.3.0-rc.4`** across all 10 deployed components (zero image substitutions; `fsb-fastlet:release-1.3.0-rc.4` runs natively on Lifsea 3) |
| Cluster & hardware | 2× ACK bare-metal nodes (`ecs.ebmg9ae.48xlarge`), 192 vCPU / 742.4 GiB allocatable RAM (~755 GiB capacity) per node, Lifsea (ContainerOS) 3, kernel `5.10.134-19.6.1.lifsea8.x86_64`, Kubernetes `v1.36.2`, `containerd://2.1.9`, `ublks_max=65535` |
| Guest network CIDR | Controller-owned `FAST_SANDBOX_NETWORK_CIDR=172.30.0.0/20` (4,094 IPs) configured on `deploy/fast-sandbox-controller` and propagated to both `sandboxtemplate-builder` and `SandboxPool` Fastlets (`fsb0=172.30.0.1/20`) |
| Template & local registry | In-cluster local `registry:2` (`opensandbox-bench-registry.opensandbox-bench-run.svc:5000`, `plainHTTP: true`), OverlayBD `Full` template (`python:3.12-bookworm` + `execd:release-1.3.0-rc.4`, `2 vCPU / 2048 MiB / 2 GiB rootfs`) |
| Prewarm & cache settings | OverlayBD `OVERLAYBD_POOL_LOW=64`, `OVERLAYBD_POOL_HIGH=80`, `OVERLAYBD_POOL_SIZE_GB=2`, `OVERLAYBD_CACHE_SIZE_GB=32`; Fastlet `FAST_SANDBOX_OCI_BLOB_CACHE_SIZE_BYTES=16 GiB`, `FAST_SANDBOX_SNAPSHOT_SPILL_DIR=/dev/shm/fsb-spill`; DART P2P disabled; client `WarmupRuns=2`, `readinessPollMs=10` |
| Telemetry & latency focus | Zero RBAC 403 warnings (`telemetry_status: "ok"`), native `janitor` `/dev/ublkb*` and `nsenter` host `conntrack` probes; create latency tables focus on `t_api_ack` and `t_first_ping` |

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

| Suite / Metric | `release-1.3.0-rc.3` | `release-1.3.0-rc.4` | Delta |
|---|---:|---:|---:|
| `schedule-burst` `t_api_ack` P50 / P95 / P99 (ms) | 294.3 / 5941.5 / 6762.5 | **72.8 / 465.6 / 930.0** | -75.3% / -92.2% / -86.2% |
| `schedule-burst` `t_first_ping` P50 / P99 (ms) | 308.1 / 6770.1 | **83.5 / 953.6** | -72.9% / -85.9% |
| `steady-churn` `t_api_ack` P50 / P95 (ms) | 191.3 / 762.7 | **43.8 / 79.8** | -77.1% / -89.5% |
| `high-density-soak` `1.00x` live sandboxes | 212 (530 denials) | **371 (0 denials, 0 OOM)** | +75.0% live, 0 denials |
| `density-ramp` single-Fastlet peak live sandboxes | 230 (`/24` limit) | **800 (`960/960` PASS, `/ping` P99 `3.16 ms`)** | 3.5× density |
| `noisy-neighbor` same-node P99 degradation ratio | 0.965× (9 victims) | **0.951× (9 victims, 0% fail)** | Stable |

## Bundle 1: Lifecycle (`schedule-burst` + `steady-churn`)

### `schedule-burst` (`576 / 768` succeeded, `75.0%`, `4.89 RPS`, `191 × http_429`, `1 × timeout`, overall `t_api_ack` P50/P95/P99 `72.8 / 465.6 / 930.0 ms`, `t_first_ping` `83.5 / 474.8 / 953.6 ms`)

| Concurrency `c` | N (OK / Fail) | Success % | Pending Fallback % | HTTP 429 | Throughput (RPS) | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P95 / P99 (ms) |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 128 / 0 | 100.0% | 0.0% | 0 | 3.11 | 65.1 / 80.0 / 86.0 | 76.3 / 90.9 / 97.0 |
| 4 | 128 / 0 | 100.0% | 0.0% | 0 | 12.74 | 57.4 / 104.7 / 120.3 | 67.4 / 115.3 / 129.4 |
| 16 | 128 / 0 | 100.0% | 0.0% | 0 | 48.02 | 59.6 / 154.0 / 182.2 | 69.1 / 163.6 / 192.7 |
| 32 | 57 / 71 | 44.5% | 0.0% | 71 | 20.01 | 184.1 / 358.7 / 381.6 | 193.2 / 370.2 / 390.0 |
| 64 | 7 / 121 | 5.5% | 12.5% | 120 | 0.12 | 947.9 / 1004.9 / 1004.9 | 957.5 / 1014.8 / 1014.8 |
| 128 | 128 / 0 | 100.0% | 0.0% | 0 | 138.98 | 383.0 / 519.1 / 529.2 | 396.8 / 591.3 / 608.2 |

### `steady-churn` (`10 RPS × 120 s`, `626 / 1200` succeeded, `52.2%`, `5.22 RPS`, `574 × http_429`)

| N (OK / Fail) | Success % | HTTP 429 | Achieved RPS | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P95 / P99 (ms) |
|---:|---:|---:|---:|---:|---:|
| 626 / 574 | 52.2% | 574 | 5.22 | 43.8 / 79.8 / 931.0 | 53.9 / 90.5 / 938.5 |

**Observed upstream defect (`fast-sandbox#289`)**: Under sustained high-concurrency create/delete churn in Bundle 1 (`c=32/64` and `steady-churn`), `fsb-fastlet:release-1.3.0-rc.4` still exhibited the `cleanSlots` leak (`capacity=120 clean=0 bound=25 destroying=1`), causing `HTTP 429` rejections.

## Bundle 2: `high-density-soak` (`732 / 734` succeeded, `99.7%`, `egressProbeUrl: "none"`)

Ran on two Fastlets (`maxSandboxesPerPod=400`, total capacity 800) against real cluster node memory (`760,205 MiB` allocatable per node).

| Oversell Ratio | Active Sandboxes | Admission Denials | OOM Kills | Firecracker PSS (MiB/sbx) | Firecracker RSS (MiB/sbx) | P99 Exec Latency (ms) | Drain Recovery (s) |
|---|---:|---:|---:|---:|---:|---:|---:|
| `0.50x` | 186 | 0 | 0 | 11.51 | 75.48 | 227.6 | 31.37 |
| `1.00x` | 371 | 0 | 0 | 10.80 | 75.81 | 220.1 | 60.99 |

## Bundle 3: Single-Fastlet `density-ramp` (`960 / 960` PASS, `placement_scope: "single_fastlet"`, `maxSandboxesPerPod=900`)

Overall `t_api_ack` P50/P95/P99 `504.9 / 2441.8 / 3065.0 ms`, `t_first_ping` `512.7 / 2448.1 / 3073.2 ms`, `density_ping` `2.5 / 6.1 / 24.1 ms`, zero failures across all 8 steps up to **800 concurrent live Firecracker MicroVMs on a single Fastlet**.

| Target Live | Delta | Batch (OK / Fail) | Step Duration (s) | `/ping` Healthy | `/ping` P99 (ms) | Host `ΔMemAvail` (MiB/sbx) | FC PSS (MiB/sbx) | FC RSS (MiB/sbx) | `ΔSlab` (MiB/sbx) | Host `load_1m` |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 50 | +50 | 50 / 0 | 0.42 | 20 / 20 | 6.14 | 11.47 | 7.60 | 42.25 | 1.64 | 0.89 |
| 100 | +50 | 50 / 0 | 0.48 | 20 / 20 | 3.43 | 13.15 | 7.86 | 46.87 | 1.98 | 1.06 |
| 150 | +50 | 50 / 0 | 0.63 | 20 / 20 | 3.65 | 14.13 | 8.51 | 53.38 | 2.16 | 3.86 |
| 200 | +50 | 50 / 0 | 0.71 | 20 / 20 | 3.94 | 14.75 | 8.87 | 58.31 | 2.27 | 3.86 |
| 230 | +30 | 30 / 0 | 0.57 | 20 / 20 | 24.32 | 15.72 | 9.39 | 64.25 | 2.32 | 3.79 |
| 400 | +170 | 170 / 0 | 2.43 | 20 / 20 | 4.32 | 16.05 | 8.80 | 60.76 | 2.43 | 3.65 |
| 600 | +200 | 200 / 0 | 5.55 | 20 / 20 | 4.61 | 17.25 | 9.34 | 66.30 | 2.54 | 6.67 |
| 800 | +200 | 200 / 0 | 7.21 | 20 / 20 | 3.16 | 18.11 | 9.68 | 70.12 | 2.59 | 9.70 |

## Bundle 4: `dataplane-microbench` & `noisy-neighbor`

- **`dataplane-microbench`** (`12 / 12` PASS on harness `f241dfe4e`, `measurement_scope: "guest_absolute"`, `11.93 s`): CPU (`python3-fallback`) single `2071.1 events/s`, 4-way concurrent mean `2224.2 events/s` (`scaling_efficiency_ratio = 1.074x`); Storage (`python3-dd-fallback`, 64 MiB buffered guest page-cache / upper-layer file I/O without `O_DIRECT`) 4K rand read `795946 IOPS`, 4K rand write `372066 IOPS`, 1M seq cold read `7190.8 MiB/s`, 1M seq hot read `19240.9 MiB/s`, 1M seq write `780.4 MiB/s`; Network (`http-probe` to `http://10.79.205.207:8080/health`) `20/20` blocked by default guest egress isolation policy (fail-closed gate & `O_DIRECT` added in PR #3176 `d5c945720`).
- **`noisy-neighbor`** (`191 / 191` PASS, `5.93 s`, single-Fastlet 100% same-node co-located `colocated_victim_count=9`, 1 aggressor [10%] + 9 victims [90%], `cpu+disk`): Baseline victim exec P50/P95/P99 `209.0 / 219.5 / 221.1 ms` vs Under-stress `208.5 / 209.6 / 210.1 ms`, **`same_node_p99_degradation_ratio = 0.951x`** (0% failure).

## Leak & Stability Audit

| Check | Status | Notes |
|---|---|---|
| Host / Pod cgroup OOM | `pass` (0 / 0) | Zero OOM kills across all 6 suites |
| Residual Sandbox / Snapshot CRs | `pass` (0 / 0) | 800-sandbox batch teardown exceeded the 30s grace window (`30.22s`) and converged cleanly to 0 afterward |
| Stuck Deleting Slots / Leaked Firecracker PIDs | `pass` (0 / 0) | Zero residual Firecracker processes |
| `cleanSlots` Conservation (Bundle 1) | **Known Defect (`fast-sandbox#289`)** | Reproduced `clean=0` under high-concurrency create/delete churn in Bundle 1; Fastlets were recreated cleanly before Bundles 2–4 |
| `ublk_devices` | `fail` (`2` across 2 nodes) | Fixed 1-device-per-node control/warm-pool delta between `/dev/ublkb*` and `QueryPoolStatus.Idle` |

## Scope & Known Limitations

This baseline evaluates lifecycle scheduling, OverlayBD warm-pool caching, high-density memory scaling up to 800 MicroVMs on one Fastlet, guest-absolute data-plane throughput, and same-node noisy-neighbor isolation. Bare-metal host-control ratios for `dataplane-microbench` and full Pause/Resume + Wake-Storm matrix runs are outside the scope of this archive.
