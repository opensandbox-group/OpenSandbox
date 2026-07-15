# Egress benchmark

**Prerequisites**: Docker, `curl` on the host. Domains: [`tests/hostname.txt`](../tests/hostname.txt) (one hostname per line; `#` and blank lines ignored). Run from `components/egress` or adjust paths.

---

## 1. `bench-dns-nft.sh`

**Compares**: plain **`curl`** container (**baseline**) → egress **`dns`** → egress **`dns+nft`**. Prints **Req/s**, **Avg**, **P50**, **P99**; percentages are vs **baseline**.

### Run

```bash
cd components/egress
./tests/bench-dns-nft.sh
```

Builds `opensandbox/egress:local` unless you set **`IMG=...`**. Optional: **`BENCH_SAMPLE_SIZE=n`** to use `n` random domains.

### View results

- **Terminal**: summary table at end.
- **Host `/tmp`**: `bench-e2e-baseline-total.txt`, `bench-e2e-dns-total.txt`, `bench-e2e-dns+nft-total.txt` (one **`time_total`** per line); `bench-e2e-{mode}-namelookup.txt`, `bench-e2e-{mode}-wall.txt`.

---

## 2. `bench-mitm-overhead.sh`

**Compares**: **`dns+nft`** without MITM vs **`dns+nft` + transparent mitmproxy**. Default **`BENCH_SCENARIOS=short,download`** — **`short`** = many HTTPS **HEAD**s; **`download`** = parallel **GET** to **`BENCH_DOWNLOAD_URL`** (default Cloudflare `__down` ~20 MiB).

### Run

```bash
cd components/egress
./tests/bench-mitm-overhead.sh
```

**`SKIP_BUILD=1`** skips image build; **`IMG`** is at the top of the script. One scenario only, e.g. **`BENCH_SCENARIOS=short`** or **`=download`**.

### View results

- **Terminal**: tables per scenario (latency / throughput vs no-MITM), plus **`E2E latency loss (avg time_total)`** in **ms/request** and **%**.
- **Host `/tmp`**:
  - Latency artifacts: `bench-mitm-*-short-*.txt`, `*-download-*.tsv`, `*-wall.txt`, etc.
  - **Container metrics** (always written): `bench-mitm-docker-stats-dns_nft.tsv`, `bench-mitm-docker-stats-dns_nft_mitm.tsv` — `unix_ts`, **`/proc/loadavg`** (load1/5/15, …), **`docker stats`** (CPUPerc, MemUsage, …). *`loadavg` inside the container often tracks the host; use for relative trends.*

---

## 3. Reference baselines (example runs)

Illustrative only — **same machine, same script**, not a SLA. **MITM** row = **`dns+nft` + transparent mitm**.

### `BENCH_SCENARIOS=download` (parallel GET, ~20 MiB, 4 streams, 1 round, 1 s sampling)

| Metric | `dns+nft` | + mitm |
|--------|-----------|--------|
| **CPUPerc** (docker) | Mostly **~2–5%**, max **~5.6%** | Often **~5–11%**, max **~10.9%** |
| **MemUsage** | **~9–18 MiB** | **~68–91 MiB** |
| **load1** | Up to **~0.23** | Spike **~0.66**, then **~0.4–0.6** |

**Takeaway**: ~**2×** peak CPU% and ~**5×** RSS vs no MITM in this trace.

### `BENCH_SCENARIOS=short` (HEAD storm; **sparse** rows if the phase is short)

Run profile (sample): `10 rounds × 40 URLs × 1 inflight = 400 requests`.

| Metric | `dns+nft` | + mitm |
|--------|-----------|--------|
| **Req/s** | **3.64** | **1.90** (**-47.6%**) |
| **Avg latency (time_total)** | **0.315 s** | **0.605 s** (**+91.9%**) |
| **P50 latency** | **0.136 s** | **0.143 s** (**+5.2%**) |
| **P99 latency** | **1.439 s** | **10.006 s** (**+595.2%**) |
| **E2E latency loss (avg)** | baseline | **+289.88 ms/request (+91.95%)** |

| Metric | `dns+nft` | + mitm |
|--------|-----------|--------|
| **CPUPerc** | Hot sample **~132%** | Hot sample **~232%** |
| **MemUsage** | **~6–10 MiB** | **~58–88 MiB** |

**`CPUPerc` > 100%** on multi-core is normal (container can use more than one core-equivalent per Docker’s metric).

**Takeaway**: this sample shows clear request-side overhead from transparent MITM, about **+289.88 ms/request** on average with throughput dropping to about half. `P50` is close to baseline while `P99` grows sharply, indicating tail-latency amplification. With only **40 requests**, tail metrics are timing-sensitive; use more rounds or more domains for stable P99.

CPU/memory trend remains consistent: peak CPU sample **~1.8×** (**232/132**), and RSS is much higher with mitmdump. For denser host/container telemetry, use longer runs or **`BENCH_DOCKER_STATS_INTERVAL=0.5`**.
