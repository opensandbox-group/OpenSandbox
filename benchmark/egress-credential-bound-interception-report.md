# Egress Credential-Bound Interception Benchmark Report

This report answers two questions about the experimental Fast Sandbox `credential-bound` interception mode (see [Egress](../docs/architecture/network/egress.md)): how much the mode adds to sandbox creation latency, and what the per-subject and process connection budgets should be. The same bench ran in two KVM integration environments (`scripts/fast-sandbox-env`): CI workflow run [44396818387](https://github.com/opensandbox-group/OpenSandbox/actions/runs/44396818387) on the self-hosted `kvm` runner (2026-11-18), and a rerun on a shared bare-metal host (2026-11-19) with the CI parameters (set A) and with larger samples (set B). Neither environment is the ACK cluster used on the [Fast Sandbox performance page](../docs/architecture/fast-sandbox/performance.md), so absolute values are not comparable with that page, and the create-latency comparison here measures the effect of the interception mode, not the ACK lifecycle path. The two environments also differ in hardware, kernel and background load; compare each environment's two legs, not absolute values across environments.

## Summary

| Measurement | CI | Bare metal, set A | Bare metal, set B |
|---|---:|---:|---:|
| M1 pooled create p50, `all` → `credential-bound` | 595.2 → 617.1 ms (+3.7%) | 696.4 → 713.9 ms (+2.5%) | 709.7 → 723.7 ms (+2.0%) |
| M2 `credential-bound` leg, passthrough minus decrypt p50 | −0.28 ms | −0.28 ms | −0.43 ms |
| M3 bytes per held bound connection, `anon` / `memory.current` | 158,100 / 158,847 | 158,582 / 165,483 | 158,231 / 165,012 |
| M4 egress CPU per request, `all` → `credential-bound` | 9.92 → 4.28 ms (−57%) | 18.34 → 8.39 ms (−54%) | 18.01 → 8.22 ms (−54%) |

What the data supports:

- **CPU.** Under a load where 95% of the new short HTTPS connections go to unbound hosts, `credential-bound` used 54–57% less egress CPU per request than `all` in all three runs. Absolute CPU per request was 1.8–2.0 times as high on the bare-metal host, so only the relative saving carries over between environments. The saving depends on the share of unbound traffic; see M4 below.
- **Memory.** An idle bound connection costs 158–166 KB of egress memory. The `anon` slope agreed within 0.3% across two hosts, two kernels and up to 4,000 connections; `memory.current`, the value the container limit enforces, was about 4% higher on the bare-metal host. The budgets below use the higher value.
- **Create latency.** In all three comparisons `credential-bound` was 14–22 ms (2.0–3.7%) higher at pooled p50. Each difference is smaller than the per-round p50 range within either leg, and the legs are separate environment bring-ups run one after the other, so the bench cannot separate a cost of the mode from run-to-run variation. An interleaved rerun in one environment (M10 under [Supplemental measurements](#supplemental-measurements)) found a mean `credential-bound` minus `all` difference of −3.4 ms with a 95% confidence interval of −40.6 to +33.8 ms, so no mode effect was detected there. The interval is wider than the 14–22 ms in question, and the ACK create path (about 100 ms) was not measured.
- **Total memory.** `all` mode costs about 165 KB of `memory.current` per held connection whether or not the host is bound; a `credential-bound` passthrough connection costs about 35 KB. With 5% of connections bound, `credential-bound` needs about 25% of the per-connection memory of `all` (see M3x).
- **Throughput.** At the default 500m egress CPU limit with keep-alive connections and 5% bound traffic, egress CPU saturated at about 110–115 requests per second in `all` mode and about 1,030–1,060 in `credential-bound` mode (see M5).
- **Not measured.** Active transfers in M3/M3x, the full 4,096-subject scale (registration time grows roughly quadratically with the subject count; see M9), and the `credential-bound` throughput ceiling at the 2-CPU limit.

## Method

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

The TLS origin fixture holds one socket per held connection, so M3 also needs the origin container's open-file soft limit well above the largest K. The container inherits the Docker daemon's default. On the bare-metal host that default was 1024, and the first `credential-bound` set A attempt reported M3 `PARTIAL`: 1,896 of 2,000 connections held and 876 alive at release. The origin's soft limit was raised to 65536 and the whole set was rerun; the `PARTIAL` attempt is not used in this report.

## Environment

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

## Results

### M1 create latency

CI, per round (milliseconds):

| Leg | Round | Success | p50 | p90 | p99 | Mean |
|---|---|---:|---:|---:|---:|---:|
| default (`all`) | 1 | 200/200 | 593.4 | 805.5 | 888.2 | 606.2 |
| default (`all`) | 2 | 198/200 | 613.1 | 812.4 | 1233.6 | 609.1 |
| default (`all`) | 3 | 198/200 | 587.4 | 745.3 | 956.5 | 588.4 |
| default (`all`) | pooled | 596/600 | 595.2 | 801.1 | 933.3 | 601.3 |
| `credential-bound` | 1 | 200/200 | 631.6 | 816.9 | 907.9 | 624.7 |
| `credential-bound` | 2 | 198/200 | 597.3 | 779.6 | 1089.9 | 617.5 |
| `credential-bound` | 3 | 198/200 | 611.6 | 794.1 | 890.6 | 606.0 |
| `credential-bound` | pooled | 596/600 | 617.1 | 798.6 | 907.9 | 616.1 |

All runs, pooled over successful creates (milliseconds):

| Run | Leg | Success | p50 | p90 | p99 | Mean | Per-round p50 range |
|---|---|---:|---:|---:|---:|---:|---:|
| CI | default (`all`) | 596/600 | 595.2 | 801.1 | 933.3 | 601.3 | 587.4–613.1 |
| CI | `credential-bound` | 596/600 | 617.1 | 798.6 | 907.9 | 616.1 | 597.3–631.6 |
| Bare metal, set A | default (`all`) | 600/600 | 696.4 | 889.2 | 1049.4 | 906.6 | 688.1–707.8 |
| Bare metal, set A | `credential-bound` | 600/600 | 713.9 | 922.6 | 1030.4 | 717.4 | 698.8–732.0 |
| Bare metal, set B | default (`all`) | 2000/2000 | 709.7 | 922.6 | 1069.3 | 714.3 | 696.2–732.3 |
| Bare metal, set B | `credential-bound` | 2000/2000 | 723.7 | 936.6 | 1056.5 | 738.7 | 696.7–741.5 |

| Run | `credential-bound` minus `all`, pooled p50 | Per-round p50 range, `all` / `credential-bound` |
|---|---:|---:|
| CI | +21.9 ms (+3.7%) | 25.7 / 34.3 ms |
| Bare metal, set A | +17.5 ms (+2.5%) | 19.7 / 33.2 ms |
| Bare metal, set B | +14.0 ms (+2.0%) | 36.1 / 44.8 ms |

CI reports M1 as `PARTIAL` in both legs. Seven of the eight failures are `SandboxRateLimitException` (HTTP 429, the integration Pool's 16 sandbox slots at concurrency 10 with asynchronous deletion); the eighth, in the third `credential-bound` round, is one `SandboxReadyTimeoutException`. Latencies cover successful attempts only. The bare-metal run had no failed create in 5,200 attempts.

Pooled percentiles hide tails concentrated in one round. In bare-metal set A, the first default round had a p99 of 31,187.8 ms and a maximum of 31,194.0 ms, which raises that leg's pooled mean to 906.6 ms; the other two rounds had a maximum of about 1,050 ms. In set B, the first `credential-bound` round had a p99 of 3,971.7 ms. The cause of these tails was not identified.

The difference between legs is not larger than the per-round p50 range of either leg in any run, and p90 and p99 do not move consistently with it. The bench therefore does not isolate a create-latency cost of the mode; the observed p50 differences of 14–22 ms are the best available estimate of its size in these environments, without a confidence interval. These creates carry a network policy but no Vault.

### M2 TLS connect latency

Guest TCP + TLS connect latency, p50 (p99), milliseconds:

| Path | CI (200 samples) | Bare metal, set A (200) | Bare metal, set B (1,000) |
|---|---:|---:|---:|
| `all_unbound` | 4.29 (6.00) | 8.02 (10.23) | 8.20 (9.60) |
| `all_bound` | 4.41 (5.78) | 8.01 (9.80) | 8.17 (9.71) |
| `cb_bound` | 4.86 (6.72) | 9.26 (13.00) | 9.39 (10.93) |
| `cb_unbound` | 4.58 (6.60) | 8.98 (11.21) | 8.96 (11.27) |

Every sample on every path, 800 in CI and 4,800 on bare metal, matched its path's expected evidence: the `all_*` and `cb_bound` samples presented the MITM leaf, the `cb_unbound` samples presented the origin leaf, and the origin saw the credential exactly on the bound paths. TCP connect took about 0.17 ms at p50 in CI and 0.35–0.39 ms on bare metal, so most of each value is the TLS handshake.

- Within the `credential-bound` leg, passthrough was 0.28, 0.28 and 0.43 ms faster at p50 than decrypt.
- Within the default leg, bound and unbound paths differed by at most 0.12 ms.
- Across legs, the `credential-bound` paths were slower at p50 than the matching default-leg paths in every run: 0.29–0.96 ms for unbound and 0.45–1.25 ms for bound, including the passthrough path. This may include a cost of classifying the ClientHello before passthrough, or differences between bring-ups; the bench does not separate the two.

The Vault relay acknowledgements recorded while binding the bench sandboxes took 48–53 ms in CI and 64–94 ms on bare metal.

### M3 egress memory

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

### M4 egress CPU

Passthrough-dominated load, 95% of requests to an unbound host:

| Run | Leg | Requests succeeded | Load CPU (cores) | Idle-corrected CPU per request | Throttled time (periods) |
|---|---|---:|---:|---:|---:|
| CI | default (`all`) | 2000/2000 | 0.109 | 9.92 ms | 163.8 ms (3) |
| CI | `credential-bound` | 2000/2000 | 0.051 | 4.28 ms | 217.1 ms (2) |
| Bare metal, set A | default (`all`) | 2000/2000 | 0.108 | 18.34 ms | 313.7 ms (7) |
| Bare metal, set A | `credential-bound` | 2000/2000 | 0.054 | 8.39 ms | 165.8 ms (9) |
| Bare metal, set B | default (`all`) | 5000/5000 | 0.108 | 18.01 ms | 813.0 ms (18) |
| Bare metal, set B | `credential-bound` | 5000/5000 | 0.054 | 8.22 ms | 1,930.1 ms (28) |

`credential-bound` used 57% (CI), 54.3% (set A) and 54.4% (set B) less egress CPU per request than `all`, because unbound connections are passed through instead of being decrypted and re-encrypted. Every request's leaf and credential evidence matched its path. Per-request CPU within one environment changed by about 2% between sets A and B. Each set has one load window per leg, so this report gives no confidence interval.

Average load stayed at or below 0.11 cores, below the 500m limit, but CFS throttling occurred in short bursts in every run. The values are CPU time, not request latency, and the load windows do not measure maximum throughput.

The saving depends on the traffic mix. Assuming that a bound request costs the same CPU in both modes, these numbers imply that a passthrough request costs 40–43% of a decrypted request in both environments. A workload whose unbound share is `u` would then save about `u × 57–60%`. This is derived from the 95/5 mix, not measured separately.

## Budget sizing

The per-subject default `OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY` is 32. The process default `OPENSANDBOX_EGRESS_FSB_PROCESS_TLS_CAPACITY` is 32768; it was lowered from 131072 in #3053 after these measurements. At 158–166 KB per held connection, 131072 connections need 20.7–21.7 GB of connection memory, far above the 256Mi, 2Gi and 8Gi egress memory limits considered in this report. The budget model, including the relationship between the per-subject and process caps, the worst-case memory each implies, and why the process budget is the bound that protects the container, is described in [Fast Sandbox networking](../docs/architecture/fast-sandbox/networking.md#interception-mode). The operator procedure and example values are in the [Credential Vault guide](../docs/guides/credential-vault.md#sizing-the-connection-budgets-operators). This section gives the measured inputs.

`m` uses the highest `memory.current` slope measured, because the container memory limit is enforced on `memory.current`, and `B` uses the highest `memory.current` baseline. A different egress image, mitmproxy version or kernel can change both values; rerun M3 before relying on them there.

| Input | Value |
|---|---|
| Bytes per bound connection `m` | 166 KB (165,483 bytes, bare-metal set A `memory.current`, rounded up) |
| Idle baseline `B` | about 70 MB (`memory.current` at K = 0: 66–70 MB) |
| `(L − B) / m` for `L` = 256Mi | about 1,195 connections |
| `(L − B) / m` for `L` = 8Gi | about 51,500 connections |

Passthrough connections are not charged to either budget, but each costs about 35 KB of `memory.current` (M3x). Leave room for them in `L − B` when a Pool expects many concurrent unbound connections. After connections close, the egress process kept their memory for the whole observation window (240 s after 2,048 connections, M7), so size `L` for the peak number of concurrent connections, not the average.

## Supplemental measurements

These measurements, labelled M3x and M5–M10, cover what the first runs did not. They ran from 2026-11-20 to 2026-11-23 on the same shared bare-metal host, with the same isolation as set B: a private Docker daemon and containerd, kind, and one cgroup capped at 24 CPUs and 64 GiB. The bench is not part of this repository; its method, parameters and acceptance rules are in [`SUPPLEMENTAL.md`](https://github.com/hpliStartAgain/OpenSandbox/blob/6bf7d46e6cf2b76564b12098d6fe1fb65e2841d1/scripts/fast-sandbox-env/SUPPLEMENTAL.md) on a reproduction branch. The measurements used three frozen versions of it, listed below. Each version passed an independent code review and an independent test pass before it ran, and all summary values in this report were recomputed independently from the raw reports. The raw reports are not published; the method and the frozen bench versions below are the reproducible record. Performance was not remeasured on current `main`.

| Measurements | Frozen bench version | Code under test |
|---|---|---|
| M5, M10 | [`1a0cc3e3`](https://github.com/hpliStartAgain/OpenSandbox/commit/1a0cc3e3276acd0ddabedbfa1a3989f78ae04bf6) | #2924 at `2a133535` |
| M3x, M6, M7 distributed bursts, M9 | [`ee85f9ae`](https://github.com/hpliStartAgain/OpenSandbox/commit/ee85f9ae25388852deb243002dae37f7939156f3) | same |
| M7 per-subject budget, M8 | [`0a5e50c5`](https://github.com/hpliStartAgain/OpenSandbox/commit/0a5e50c593bcacbe311563dd67f0b813298c47e4) | same |

The reproduction branch ([`6bf7d46e`](https://github.com/hpliStartAgain/OpenSandbox/tree/6bf7d46e6cf2b76564b12098d6fe1fb65e2841d1/scripts/fast-sandbox-env)) is the last frozen version rebased onto current `main`, with fixes to M7 fill accounting, refill handling and boundary-probe validation. Each mode switch rebuilds the Pool, so different modes never share an egress process. The egress CPU limit is the default 500m unless stated otherwise. The ACK create path was not measured.

### M3x total memory

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

### M5 throughput and latency

Closed-loop load from 4 guests: concurrency 1, 16, 64 and 256 in total, a 10-second warmup and a 60-second steady window, 3 repeats per point. The grid has 288 runs: 2 modes × 2 egress CPU limits (500m and 2 CPUs) × 2 connection shapes (new connection per request and keep-alive) × 3 traffic profiles × 4 concurrency levels × 3 repeats. The three traffic profiles are 5% bound with 0-byte responses, 5% bound with 64 KiB responses, and 100% bound with 0-byte responses; 100% bound with 64 KiB responses was not run. The table shows 500m, keep-alive, 0-byte responses and 5% bound traffic, as mean RPS and the mean of p50 and p99 in milliseconds over the 3 repeats:

| Total concurrency | `all` RPS | `all` p50 / p99 | `credential-bound` RPS | `credential-bound` p50 / p99 |
|---:|---:|---:|---:|---:|
| 1 | 22.1 | 46.0 / 47.2 | 23.9 | 42.8 / 46.2 |
| 16 | 115.1 | 119.1 / 213.9 | 368.1 | 42.5 / 90.0 |
| 64 | 110.3 | 523.7 / 3,338.6 | 1,061.9 | 54.4 / 108.1 |
| 256 | 77.4 (1,025 errors) | 710.1 / 19,304.8 | 1,028.4 (73 errors) | 189.9 / 406.2 |

In both modes the egress container used its full 0.5 core from concurrency 16 (`all`) or 64 (`credential-bound`) onward, with CFS throttling in almost every period. Over the same windows the client and the origin each used at most about 0.45 core while egress stayed pinned at its limit, so the egress CPU limit is the likely bottleneck. The plateau is about 110–115 RPS for `all` and 1,030–1,060 RPS for `credential-bound`, about nine times as high. At the 2-CPU limit, `all` levelled off at about 230 RPS. `credential-bound` reached about 1,980 RPS at concurrency 256 with no errors and no plateau, so its 2-CPU ceiling was not established. Single-connection latency was 43–46 ms at p50 in both modes; this floor was not investigated.

### M6 connection churn

Each mode ran for 1,800 seconds: repeated connect, request and close cycles at concurrency 1, 4 and 16 for 600 seconds each, sampled every 5 seconds. Neither mode had a failed request. Within each concurrency segment, the p50 moved by −5.8% to +0.8% between the first and last minute. Egress file descriptors returned to the baseline after the load. Memory stayed about 33 MB above the baseline 30 seconds after the load stopped; see the retention note under M7.

### M7 bursts and the per-subject budget

Simultaneous bursts of B = 500, 1000, 2000 and 2049 bound connections were split across 6 sandboxes, with 3 repeats per tier. Each sandbox is a separate subject, so no subject reached its budget in this part.

| Mode | Attempted | Failed | Client-side failure kinds |
|---|---:|---:|---|
| `all` | 16,647 | 150 (0.9%) | 139 receive errors, 11 handshake resets |
| `credential-bound` | 16,647 | 958 (5.8%) | 386 receive timeouts, 521 receive errors, 51 handshake resets |

There was no budget rejection. Passthrough traffic kept working, and resources recovered after every burst. In `credential-bound` mode, 575 of the 958 failures came in the third repeat of a tier. The failure timing is consistent with admission queueing beyond the client timeout during back-to-back bursts; the cause was not isolated, and whether egress has a serial point there was not determined.

To test the per-subject budget itself, one sandbox with 2 GiB of memory filled its subject to the budget of 2048 in batches of 32. Each batch was verified at the client and counted at the origin, so 2,048 connections were confirmed held before one more bound connection was attempted:

| | `all` (no per-subject budget) | `credential-bound` |
|---|---|---|
| Connections held / alive at release | 2,048 / 2,048 | 2,048 / 2,048 |
| Connection 2,049 (bound) | succeeded | rejected at the TLS handshake (reset) |
| `registry_exhausted` events in the probe window | 0 | 1 |
| Unbound request from the same sandbox while full | succeeded | succeeded |
| Credential injection after release | recovered | recovered |

Budget exhaustion therefore rejected only the new bound handshake. Existing connections and passthrough traffic from the same subject were unaffected. 240 seconds after release, file descriptors and conntrack entries had returned to the baseline, but `anon` stayed 299 MiB (`all`) and 303 MiB (`credential-bound`) above it, about the memory of 2,048 connections. These observations are consistent with allocator retention, and M8 showed no continuing growth over two hours of sustained churn. Whether the memory is returned under host memory pressure was not tested.

The `registry_exhausted` log line reached the container log about 34 seconds after the rejection, because egress reads the interception proxy's output through a pipe without forcing a flush. The measurement set `PYTHONUNBUFFERED=1` on this Pool only, so that the log line appears at the time of the rejection.

### M8 two-hour soak

Each mode ran for 7,200 seconds at concurrency 16 with keep-alive connections, 5% bound traffic and a reconnect every 100 requests, sampled every 10 seconds. The target rates were half of the 500m plateau from M5. Trends use the last 90 minutes:

| | `all` | `credential-bound` |
|---|---:|---:|
| Target / achieved RPS | 55 / 53.15 | 514 / 223.37 |
| Requests succeeded / failed | 382,679 / 0 | 1,608,286 / 0 |
| p50 / p99 | 6.9 / 86.4 ms | 42.5 / 65.6 ms |
| p50 change, first to last minute | −2.2% | +0.4% |
| `anon` / `memory.current` slope | 29 / 78 B/s | −0.9 / 29 B/s |
| Peak egress `memory.current` | 102 MB | 116 MB |
| Peak egress file descriptors | 99 | 100 |
| Restarts / OOM kills | 0 / 0 | 0 / 0 |

The `credential-bound` run did not reach its target. The load generator paces each request with a fixed sleep on top of the request time, which limits 16 workers to about 224 RPS; without pacing, 16 workers at 42.5 ms would still stay below 514. The result shows two hours of stability at 223 RPS, about four times the `all` load, not at 514 RPS. A slope of 78 B/s over 90 minutes is about 0.4 MB, so neither mode showed memory growth.

### M9 subject scale

The `credential-bound` mode registered synthetic subjects through the action endpoint, driven from outside the egress cgroup, toward targeted totals of 256, 1,024, 2,048, 4,096 and 4,097. At each total, two real sandboxes sent 20 unbound and 20 bound requests. All 120 probe requests at 256, 1,024 and 2,048 subjects succeeded on the expected path.

`SET_BINDING` time grows with the number of subjects already installed. Almost all of it is spent installing the interception redirect for the new subject; policy commit time does not change:

| Installed subjects | `SET_BINDING` p50 | Interception redirect install p50 |
|---:|---:|---:|
| 0–300 | 140 ms | 83 ms |
| 900–1,100 | 3.0 s | 2.7 s |
| 1,900–2,100 | 12.5 s | 11.8 s |
| 2,600–2,800 | 22.7 s | 21.7 s |

Over 2,790 registrations, a quadratic fit (r² 0.986) describes the growth better than a linear one (r² 0.917). It extrapolates to about 53–55 seconds per registration at 4,096 subjects and about 14 hours to register subjects 2,792 to 4,097. The installed redirect count reached 2,792, and the largest fully probed tier is 2,048. The run stopped when the bench's 4-hour wait for the 4,096 tier expired, so the 4,096 tier and the capacity response at 4,097 were not observed. Per-tier probe reports were kept as summaries only. Synthetic subjects do not reproduce real sandbox topology. The growth curve is the main result: at a few thousand subjects per egress process, binding a new subject takes seconds to tens of seconds.

### M10 interleaved create latency

This run used one kind cluster, one image and one template. The interception mode alternated in 8 blocks in the order ABBAABBA, where A is `all` and B is `credential-bound`, with a Pool rebuild at each switch. Each block ran M1 with 3 rounds of 200 creates at concurrency 10. Blocks are the independent unit.

| Adjacent A/B pair | 1 | 2 | 3 | 4 |
|---|---:|---:|---:|---:|
| B − A, block p50 | −33.5 ms | −4.3 ms | +0.9 ms | +23.4 ms |

The mean difference is −3.4 ms, with a 95% confidence interval (Student t, 4 pairs) of −40.6 to +33.8 ms. The first round of each block had 6 `SandboxRateLimitException` failures, 48 of 4,800 creates in total; they shift a block p50 by at most 3.8 ms. No mode effect is detected, but the interval is too wide to exclude a difference of 14–22 ms. Settling it on a create path of about 100 ms needs a run on that path.

## Reproducing

Run after `up` and the verify actions, with the same `EGRESS_INTERCEPTION_MODE` as `up`. Reports are written under `$WORK/bench/<leg>/` (`m1-create.json`, `m2-handshake.json`, `m3-memory.json`, `m4-cpu.json`, `environment.json`, `bench-summary.json`):

```bash
./scripts/fast-sandbox-env/integration-env.sh bench-credential-bound
EGRESS_INTERCEPTION_MODE=credential-bound ./scripts/fast-sandbox-env/integration-env.sh bench-credential-bound
```

The `bench-credential-bound` action and its CI wiring were added by #3054 (framework, M1 and M2) and #3062 (M3 and M4). In CI, the bench runs when the `bench` dispatch input is set or the pull request carries the `fast-sandbox-bench` label: the default-mode bench runs before the workflow switches the Pool to `credential-bound`, and the `credential-bound` bench runs after the switch. The reports are uploaded as `fast-sandbox-bench-<leg>-<attempt>` artifacts. The `BENCH_*` variables listed in the [environment README](https://github.com/opensandbox-group/OpenSandbox/blob/main/scripts/fast-sandbox-env/README.md) change sample counts, steps and the M3 render. The defaults are set A; set B in this report used:

```bash
BENCH_CREATE_ROUNDS=5 BENCH_CREATE_N=400 BENCH_HANDSHAKES=1000 \
BENCH_MEM_STEPS="500 1000 2000 4000" BENCH_FSB_TLS_CAPACITY=2048 \
BENCH_CPU_UNBOUND=4750 BENCH_CPU_BOUND=250 \
  ./scripts/fast-sandbox-env/integration-env.sh bench-credential-bound
```

Run both legs in the same environment and with the same parameters, and save each leg's reports before the next bring-up. To compare modes on a host with background load, repeat the pair of legs more than once rather than relying on one pair. Check the Docker daemon's default open-file limit before M3; see [Method](#method).

The supplemental measurements M3x and M5–M10 are not part of this repository or the CI workflow. Their entry, parameters and report format are described in [`SUPPLEMENTAL.md`](https://github.com/hpliStartAgain/OpenSandbox/blob/6bf7d46e6cf2b76564b12098d6fe1fb65e2841d1/scripts/fast-sandbox-env/SUPPLEMENTAL.md) on the reproduction branch. M8 needs the target rates chosen from an M5 run in the same environment. M9 at 4,096 subjects needs a much longer wait per tier than the default (see M9).
