---
title: Fast Sandbox Performance
description: ACK OverlayBD performance measurements for fast-sandbox — serial and burst concurrent creates up to c=256, throughput, warm pool hit rate, and internal runtime phase breakdown.
---

# Performance

ACK (Alibaba Cloud Container Service for Kubernetes) is Alibaba Cloud's managed Kubernetes service. This page reports OverlayBD-backed fast-sandbox measurements collected on a two-node ACK cluster on 2026-11-27 across four consecutive benchmark cycles totaling **3,744 / 3,744 sandboxes (100% pass, 0 errors, 0 HTTP 429)** from serial (`c=1`) through burst concurrency (`c=256`). Internal server, Fastlet, egress, and `overlaybd-ublkd` phase timings are reported alongside end-to-end latency and throughput. These are engineering measurements, not a production SLA.

The warm create path admits work into pre-warmed Fastlet capacity (see [Scheduling](/architecture/fast-sandbox/scheduling)), acquires a pre-warmed OverlayBD ublk rootfs device from `overlaybd-ublkd`, and restores a Firecracker snapshot instead of booting a kernel (see [Firecracker](/architecture/fast-sandbox/firecracker)).

::: info Key highlights (4-cycle aggregate, 3,744 / 3,744 sandboxes)
- **100.0% pass rate** across all 24 runs (3,744 / 3,744 sandboxes, 0 errors, 0 HTTP 429).
- **100.0% OverlayBD warm pool hit rate** across all 3,792 measured and warmup creations (0 pool misses).
- **Serial latency (`c=1`, 2,000 samples)**: `t_api_ack` P50 **75.3 ms** (P99 **107.3 ms**); `t_first_ping` P50 **86.4 ms** (P99 **118.6 ms**).
- **Peak burst throughput (`c=128`)**: **127.71 RPS** 4-cycle mean, peaking at **151.56 RPS** (P99 `t_api_ack` **482.3 ms** in Cycle 3).
- **High-concurrency burst (`c=256`)**: **1,024 / 1,024** succeeded at **103.83 RPS** mean (peak **113.69 RPS**), with `ublkdAcquire` P99 at **14.68 ms**.
- **Runtime stability**: **0 OOM events**, **0 pod restarts**, and **0 residual ublk devices or Firecracker PIDs** across all four cycles.
:::

## How to read these numbers

- **`t_api_ack`** measures end-to-end `POST /v1/sandboxes` creation latency until HTTP `202` acknowledgement from `opensandbox-server` (through FastPath and Fastlet runtime creation).
- **`t_first_ping`** measures end-to-end readiness from creation start until the first guest `/ping` HTTP `200` through the ingress path.
- **Throughput (RPS)** is measured over each concurrency burst (`N` concurrent creates divided by total wall time to completion).
- **OverlayBD warm pool hit rate** counts how many creations acquired a ready ublk block device directly from the `overlaybd-ublkd` warm pool (`--pool-low 64 --pool-high 80`) rather than waiting for on-demand device setup. Each run includes two warmup creates in the pool hit total.

[Download the raw benchmark report](/benchmarks/fast-sandbox-ack-overlaybd-2026-11-27.json) for structured environment metadata, 4-cycle aggregates, per-cycle (C1–C4) tables, and internal phase breakdowns.

## Environment

| Dimension | ACK OverlayBD environment (2026-11-27) |
|---|---|
| Cluster | 2 ACK nodes (`ecs.ebmg9ae.48xlarge`) |
| Fastlet pool | `opensandbox-bench-pool`, 2/2 Fastlets (1 per node), `ResourceAware`, `overcommitRatio: 8:4`, `maxSandboxesPerPod: 500` |
| Network | `FAST_SANDBOX_NETWORK_CIDR=172.30.0.0/21` (2,048 IPs) |
| Template | OverlayBD `tpl-c42b514b-81dc-4eee-b1a6-3b0f24da075a` (`python:3.12-bookworm`, 2 vCPU, 2048 MiB memory, 2 GiB rootfs) |
| `overlaybd-ublkd` | Photon WorkPool with **16 worker threads** (`--pool-low 64 --pool-high 80`) |
| `opensandbox-server` | Single replica (`docker.io/opensandbox/server:aio-355e90d1d`) |
| `fast-sandbox` components | `fsb-controller`, `fsb-fastlet`, `fsb-fastlet-proxy`, `fsb-janitor`, `fsb-sandboxtemplate-builder` (`dev-a6b2a3e`) |
| `firecracker-runtime` | `docker.io/opensandbox/fsb-firecracker-runtime:dev-52c1c54` |
| `egress` | `docker.io/opensandbox/egress:perf-nft-77ab98372` |

## 4-cycle aggregate results

Each cycle runs six concurrency tiers (`c1_n500`, `c4_n4`, `c16_n16`, `c32_n32`, `c128_n128`, `c256_n256`) for 936 timed sandboxes per cycle and **3,744 sandboxes** across Cycles 1–4. All 24 runs achieved **100% pass rate**, **0 HTTP 429**, and **100.0% OverlayBD warm pool hit rate**.

| Tier | Pass rate | RPS mean (min–max) | `t_api_ack` P50 mean (range) | `t_api_ack` P95 mean (range) | `t_api_ack` P99 mean (range) | `t_first_ping` P50 / P95 / P99 mean | OverlayBD pool hit |
|---|---:|---:|---:|---:|---:|---:|---:|
| `c1_n500` | 2,000 / 2,000 (100%) | 3.09 (3.05–3.11) | 75.3 ms (72.7–81.5) | 98.5 ms (95.1–102.8) | 107.3 ms (104.4–110.7) | 86.4 / 109.8 / 118.6 ms | 100.0% (2,008 / 2,008) |
| `c4_n4` | 16 / 16 (100%) | 10.98 (8.86–11.99) | 90.5 ms (80.2–113.3) | 95.8 ms (83.5–116.6) | 95.8 ms (83.5–116.6) | 99.5 / 104.9 / 104.9 ms | 100.0% (24 / 24) |
| `c16_n16` | 64 / 64 (100%) | 40.80 (37.57–44.62) | 122.5 ms (96.8–156.3) | 139.6 ms (111.2–177.3) | 139.6 ms (111.2–177.3) | 132.0 / 148.1 / 148.1 ms | 100.0% (72 / 72) |
| `c32_n32` | 128 / 128 (100%) | 65.62 (61.54–72.96) | 164.8 ms (131.3–190.1) | 215.9 ms (165.7–255.2) | 223.5 ms (167.1–256.7) | 181.8 / 226.9 / 233.8 ms | 100.0% (136 / 136) |
| `c128_n128` | 512 / 512 (100%) | **127.71** (88.08–151.56) | 393.8 ms (320.5–589.3) | 720.9 ms (472.6–1170.0) | 737.8 ms (482.3–1206.1) | 451.2 / 753.1 / 768.7 ms | 100.0% (520 / 520) |
| `c256_n256` | 1,024 / 1,024 (100%) | **103.83** (80.72–113.69) | 893.3 ms (685.0–1303.2) | 1912.7 ms (1434.4–2868.0) | 2037.5 ms (1548.6–2904.3) | 986.2 / 1926.9 / 2047.2 ms | 100.0% (1,032 / 1,032) |

In addition to the 4-cycle suite, a standalone `c=64` (`c64_n64`) reference run on the same stack completed **64 / 64 (100%)** in **0.59 s** (**108.75 RPS**), with `t_api_ack` P50/P95/P99 at **232.9 / 290.9 / 296.5 ms**, `t_first_ping` P50/P95/P99 at **242.3 / 300.0 / 304.9 ms**, and **100.0% (66 / 66)** OverlayBD pool hit rate.

## Per-cycle breakdown (Cycles 1–4)

Every run across Cycles 1–4 recorded **100% pass rate** and **100.0% OverlayBD warm pool hit rate**, with **0 OOM events**, **0 pod restarts**, and **0 residual ublk devices or Firecracker PIDs**.

| Cycle | Tier | RPS | `t_api_ack` P50 / P95 / P99 (ms) | `t_first_ping` P50 / P99 (ms) |
|---|---|---:|---:|---:|
| C1 | `c1_n500` | 3.10 | 73.2 / 95.1 / 104.4 | 84.2 / 115.2 |
| C1 | `c4_n4` | 8.86 | 81.4 / 93.0 / 93.0 | 90.9 / 100.4 |
| C1 | `c16_n16` | 38.18 | 126.0 / 142.6 / 142.6 | 134.8 / 150.8 |
| C1 | `c32_n32` | 62.53 | 190.1 / 255.2 / 256.7 | 201.7 / 266.9 |
| C1 | `c128_n128` | 121.31 | 325.0 / 661.1 / 664.1 | 416.7 / 750.6 |
| C1 | `c256_n256` | 108.83 | 835.2 / 1851.8 / 2093.4 | 976.8 / 2104.7 |
| C2 | `c1_n500` | 3.11 | 72.7 / 98.0 / 107.6 | 83.9 / 119.6 |
| C2 | `c4_n4` | 11.94 | 80.2 / 83.5 / 83.5 | 90.9 / 91.6 |
| C2 | `c16_n16` | 42.82 | 110.9 / 127.2 / 127.2 | 120.9 / 137.8 |
| C2 | `c32_n32` | 65.44 | 131.3 / 202.9 / 228.6 | 149.5 / 239.3 |
| C2 | `c128_n128` | **149.88** | 320.5 / 579.8 / 598.8 | 411.8 / 612.0 |
| C2 | `c256_n256` | 112.08 | 749.9 / 1496.6 / 1603.8 | 858.7 / 1610.9 |
| C3 | `c1_n500` | 3.11 | 73.7 / 98.3 / 106.5 | 85.0 / 117.2 |
| C3 | `c4_n4` | 11.99 | 87.0 / 89.9 / 89.9 | 93.9 / 99.7 |
| C3 | `c16_n16` | 44.62 | 96.8 / 111.2 / 111.2 | 105.6 / 119.6 |
| C3 | `c32_n32` | 72.96 | 148.2 / 165.7 / 167.1 | 174.3 / 179.8 |
| C3 | `c128_n128` | **151.56** | 340.4 / 472.6 / 482.3 | 366.3 / 497.5 |
| C3 | `c256_n256` | **113.69** | 685.0 / 1434.4 / 1548.6 | 793.3 / 1559.4 |
| C4 | `c1_n500` | 3.05 | 81.5 / 102.8 / 110.7 | 92.4 / 122.4 |
| C4 | `c4_n4` | 11.14 | 113.3 / 116.6 / 116.6 | 122.4 / 127.8 |
| C4 | `c16_n16` | 37.57 | 156.3 / 177.3 / 177.3 | 166.6 / 184.2 |
| C4 | `c32_n32` | 61.54 | 189.7 / 240.0 / 241.7 | 201.5 / 249.4 |
| C4 | `c128_n128` | 88.08 | 589.3 / 1170.0 / 1206.1 | 610.1 / 1214.6 |
| C4 | `c256_n256` | 80.72 | 1303.2 / 2868.0 / 2904.3 | 1316.0 / 2913.8 |

## Internal runtime phase breakdown

To isolate control-plane, network-binding, and OverlayBD/Firecracker data-plane costs under burst load, internal stage timings were collected from `opensandbox-server`, `fsb-fastlet`, `egress`, and `overlaybd-ublkd` logs during dedicated `c=128` (130 OK including 2 warmups; 70 / 60 across the 2 nodes, **129.46 RPS**, 0.99 s total) and `c=256` (258 OK including 2 warmups; 132 / 126 across the 2 nodes, **118.48 RPS**, 2.16 s total) runs on the same cluster stack.

| Phase | `c=128` P50 / P95 / P99 (max) | `c=256` P50 / P95 / P99 (max) | Component |
|---|---:|---:|---|
| `t_api_ack` (client end-to-end) | 532.3 / 717.3 / 730.9 ms | 717.4 / 1536.9 / 1666.8 ms | Client → Server → Fastlet |
| `t_first_ping` (client readiness) | 544.5 / 746.2 / 751.3 ms | 885.8 / 1583.8 / 1678.0 ms | Client → Ingress → Guest `/ping` |
| `fastpath_rpc` | 318.0 / 509.0 / 537.0 ms (538.0 ms) | 134.0 / 309.0 / 346.0 ms (399.0 ms) | `opensandbox-server` → FastPath |
| Server self-overhead (`total - fastpath_rpc`) | 0.0 / 0.0 / 0.0 ms (1.0 ms) | 0.0 / 0.0 / 0.0 ms (0.0 ms) | `opensandbox-server` |
| `fastlet total` | 189.0 / 350.0 / 359.0 ms (365.0 ms) | 109.0 / 278.0 / 328.0 ms (366.0 ms) | `fsb-fastlet` |
| `runtimeEnsure` | 172.0 / 303.0 / 314.0 ms (339.0 ms) | 63.0 / 169.0 / 191.0 ms (214.0 ms) | `fsb-fastlet` → `firecracker-agent` |
| `ublkdAcquire` | **7.08 / 33.66 / 37.68 ms** (40.67 ms) | **4.54 / 7.96 / 14.68 ms** (18.26 ms) | `overlaybd-ublkd` (16-worker Photon WorkPool) |
| `set_binding` | 8.0 / 27.0 / 33.0 ms (36.0 ms) | 9.0 / 49.0 / 65.0 ms (67.0 ms) | `egress` (`nftables` binding) |
| `dispatchHooks` | 0.0 / — / 1.0 ms | 0.0 / 0.0 / 0.0 ms (1.0 ms) | `fsb-fastlet` |

## Interpreting the results

- **OverlayBD warm pool eliminates rootfs bottlenecks**: With `overlaybd-ublkd` running a 16-worker Photon WorkPool and `--pool-low 64 --pool-high 80`, background pool replenishment keeps pace with 128- and 256-way bursts, sustaining **100.0% warm pool hit rate** across all 3,792 creations and holding `ublkdAcquire` P99 to **14.68–37.68 ms**.
- **Low egress and server overhead**: `opensandbox-server` self-overhead is **0.0 ms** at P50/P95/P99 once requests enter the handler, and nftables-based `egress` `set_binding` stays at **8.0–9.0 ms** P50 (**33.0–65.0 ms** P99) under burst concurrency.
- **Minimal readiness gap after API ack**: Across all tiers, `t_first_ping` tracks `t_api_ack` closely (for example, **86.4 ms** vs **75.3 ms** P50 at `c=1`, and **451.2 ms** vs **393.8 ms** P50 at `c=128`), confirming that restored microVMs become reachable through the ingress path immediately after creation acknowledgement.
- **Single-replica server HTTP accept queuing at `c=256`**: At `c=256`, the data plane stays well under 400 ms (`runtimeEnsure` P99 **191.0 ms**, `fastpath_rpc` P99 **346.0 ms**), whereas client-observed `t_api_ack` P99 reaches **1,666.8 ms**. That ~1.3 s gap reflects HTTP accept and request queuing in front of the single-replica `opensandbox-server` rather than OverlayBD or Firecracker saturation; scaling `opensandbox-server` replicas horizontally addresses entry-side queuing for bursts above `c=128`.

::: tip Capacity planning note
Because two ACK nodes with `overlaybd-ublkd` WorkPool sustain >125 RPS average (up to **151.56 RPS** peak) at `c=128` while keeping `opensandbox-server` at a single replica, scale `opensandbox-server` replicas alongside Fastlet nodes when targeting sustained burst concurrencies of `c=256` or higher.
:::
