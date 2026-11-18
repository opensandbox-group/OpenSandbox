---
title: Density Tradeoff Benchmark
description: Reproduce density-versus-cost measurements for Firecracker-scale sandbox fleets — oversell sweeps, red-line posture, elastic sigma-caps invariant, tighten-ordering cases, and a read-only live-node sampler.
---

# Density Tradeoff Benchmark

The opt-in [runner](https://github.com/opensandbox-group/OpenSandbox/blob/main/tests/python/benchmarks/density_tradeoff.py) explores the frontier between memory density and its operating cost for micro-VM sandbox fleets. A density tier is an **oversell ratio**: the sum of declared sandbox memory caps divided by node memory. Each tier reports resident memory, PSI pressure, MemAvailable headroom, admission denials, red-line activity, and OOM/eviction events.

The runner is the reproducible evidence companion for the density and oversubscription proposal [OSEP-0033](https://github.com/opensandbox-group/OpenSandbox/pull/2915) and its Test Plan. It complements the Test Plan's acceptance runs; it does not replace them.

## Backends

| | `sim` (default) | `host` |
| --- | --- | --- |
| What it does | Deterministic fleet model calibrated by the proposal's evidence; drives the red-line state machine, the sigma-caps invariant, balloon tighten plans, and `deflate_on_oom` | Read-only sampler for a live Linux node (`/proc/meminfo`, `/proc/pressure`, `/proc/vmstat`, cgroup v2 memory accounting) |
| Needs | Python 3.10+, no third-party packages | A Linux node and a fleet brought up by your own driver |
| Output | Modeled projections | Measurements |
| Runs in CI | Yes, offline | No; needs a running fleet |

::: warning
`sim` numbers are projections produced by a calibrated model. They reproduce the shape of the measured frontier and exercise the policy layers, but they are not measurements. Use `host` or the Test Plan's acceptance suites for measured evidence.
:::

## Prerequisites

- Python 3.10 or newer. The runner itself has no third-party dependencies; the offline tests use the `tests/python` project environment.
- For the `host` backend: Linux with `/proc/meminfo`, `/proc/pressure/*`, `/proc/vmstat`, and cgroup v2 if you want per-sandbox accounting.

The harness never creates, configures, pauses, or destroys sandboxes in either backend.

## Run the density sweep

```bash
cd tests/python
python3 benchmarks/density_tradeoff.py \
  --tiers 0.5,1,2,4 \
  --posture both \
  --mix idle=0.8,sparse=0.1,bursty=0.1 \
  --duration 300 --interval 1 --seed 7 \
  --report density-report.md \
  --out-jsonl density-samples.jsonl \
  --out-summary density-summary.json
```

The report has one section per posture:

- `redline` — no invariant; placement is limited only by the red-line bands and `deflate_on_oom`. This is the posture behind the 4000-sandbox idle-sparse evidence.
- `elastic` — a declared floor/cap tier plus the host-wide sigma-caps invariant: the sum of declared caps stays within 80% of node memory, with the platform baseline charged against the same budget, so simultaneous full inflation fits by construction.

Every row shows the requested and placed oversell ratio, the placed sandbox count, per-sandbox resident memory, peak resident share of the node, minimum MemAvailable, peak modeled PSI, red-line events, tighten actions and skips, guest and host OOM counts, deflate saves, snapshot storage, and a verdict. The verdict fails on host OOM kills, guest OOM kills, memory exhaustion, an unrecovered red line, or storage over the configured limit. During recovery the drain resumes frozen guests in batches only while the projected resumption growth keeps the node above the red-line enter band; guests that do not fit stay frozen and are retried on later ticks.

### Options that change the answer

| Option | Default | Meaning |
| --- | --- | --- |
| `--tiers` | `0.5,1,2,4` | Oversell ratios to sweep; sandbox count is derived from node memory and cap |
| `--cap-mib`, `--floor-mib` | `512`, `256` | Declared cap and elastic floor per sandbox; the floor must cover the modeled ~23 MiB boot footprint |
| `--mix` | `idle=0.8,sparse=0.1,bursty=0.1` | Workload mix; also accepts `dense` for full-inflation runs. Every guest's clock starts at its own admission, so a dense guest admitted mid-run ramps at the profile rate rather than materializing a full working set |
| `--node-memory-mib`, `--platform-reserved-mib`, `--physical-cores` | evidence-class node | Node shape; the reserve is charged against MemAvailable and the invariant |
| `--duration`, `--interval`, `--seed` | `300`, `1`, `7` | Horizon and determinism |
| `--storage-gib`, `--storage-headroom-pct` | `3500`, `10` | Snapshot storage axis; the first wall in the measured evidence |
| `--red-line-enter-pct`, `--red-line-exit-pct` | `15`, `18` | Node MemAvailable bands with hysteresis |
| `--invariant-pct` | `80` | Sigma-caps budget for the elastic posture |
| `--tighten-margin-mib` | `512` | Retained-memory margin above touched memory during tighten |
| `--host-oom-reserve-mib` | scaled | Last-resort eviction threshold; the default is 0.8% of node memory, floored at 256 MiB and capped at 90% of the red-line enter band, so it never sits above the red-line bands on a small node (the cap can pull it below the floor there) |
| `--inflate-first`, `--no-deflate-on-oom`, `--no-floor-guarantee` | off | Deliberately unsafe policy variants for comparison |

## Full-inflation demonstration

```bash
python3 benchmarks/density_tradeoff.py --tiers 0.5,1,2 --mix dense=1.0
```

Idle-heavy fleets do not make memory the binding axis; this run does. At 1× and above the red-line-only posture cannot reclaim anything at 512 MiB caps, because every tighten floor exceeds the cap and the plan emits skips. The red line stays engaged and the row fails with `red_line_unrecovered`. The elastic posture caps placement at the invariant and passes at every requested ratio. That is the proposal's trade in one table: steady state by invariant, bursts by the red line; density or the guarantee, picked per pool.

## Tighten-ordering matrix

```bash
python3 benchmarks/density_tradeoff.py --scenario tighten-margins
```

The matrix mirrors the Test Plan's integration cases: a 512 MiB margin holds with zero guest OOM, the known-bad 128 MiB margin keeps the failure visible, an infeasible floor emits a skip instead of an inflate, and inflate-before-throttle ordering OOMs guests that are still allocating. Each row prints the expected outcome and a pass/fail verdict against it. The synthetic red-line trigger is pinned to elapsed time (25 s, or 10 s for the inflate-first case), so `--interval` changes the sampling cadence rather than the case; a horizon shorter than a case needs is extended so the forced window and its release always fit.

## `deflate_on_oom` rescue

```bash
python3 benchmarks/density_tradeoff.py --scenario deflate-rescue
```

A burst beyond the balloon-reduced effective cap must be absorbed by automatic deflation. With the escape hatch disabled the same burst kills the guest workload, and the row says so.

## Sample a live node

```bash
python3 benchmarks/density_tradeoff.py --backend host \
  --cgroup-root /sys/fs/cgroup/fastsandbox \
  --cap-mib 512 --duration 600 --interval 10 \
  --out-jsonl host-samples.jsonl --report host-report.md
```

Child directories of `--cgroup-root` are treated as sandboxes; `memory.current`, `memory.max`, `memory.events`, and `memory.pressure` are read for each. Without a cgroup root, pass `--sandboxes N` to report node-level signals only; residency and cgroup OOM fields are then reported as `n/a` rather than zero, and `--sandboxes` only feeds the oversell ratio. The two options are mutually exclusive: with a cgroup root the sandbox count comes from the listing, so an override would contradict it. Bring the fleet up with your own driver and run the sampler per density tier; the sampler is intentionally driver-agnostic.

The report separates OOM sources: `system OOM delta` comes from `/proc/vmstat` and includes cgroup-contained kills, `cgroup OOM delta` comes from the fleet's `memory.events`, and `host OOM delta` is the system delta minus the fleet cgroup delta (floored at zero) — the node-level share. Cgroup counters are tracked per directory inode, so a sandbox recreated at the same path between two polls counts its fresh counter from zero instead of losing kills to the old value.

## Output schema

- `--out-jsonl`: one `sample` record per simulated tick (or host sample), tagged with its tier or matrix case, plus one `summary` or `matrix-row` record per tier or case — all JSON lines.
- `--out-summary`: a JSON array of tier (or matrix row) summaries.
- `--report`: the markdown table printed to stdout.
- Every simulator summary carries a `config` block with the full effective node shape, policy bands, and storage settings, so a saved run can be reproduced without re-stating overrides alongside it.

## Calibration and provenance (`sim` backend)

Every model constant is a named default with a source in the proposal's evidence section. Change them with the options above when your fleet or node class differs.

| Parameter | Default | Provenance |
| --- | --- | --- |
| Node shape | 515745 MiB, 128 physical cores, 110 GiB platform reserve | Evidence-class bare-metal node in OSEP-0033 (~504 GiB) |
| VMM overhead / idle anonymous / image file pages | 3 / 2.2 / 21 MiB | E1: ~26.35 MB idle residency, ~2.1 MB anonymous, rest file-backed |
| File-backed reclaimable share | 0.85 | E1: 4000 idle sandboxes lowered MemAvailable by ~32 GB, not by their full RSS |
| Free-page-reporting delay | 6 s | E3: freed pages returned to the host within ≤ 6 s |
| Idle CPU | 0.3 mcore per sandbox | E1/E2: 0.29–0.30 mcore per idle sandbox |
| PSI `cpu some` model | 55 × utilization² | E2: 6.7% `some` at 45 busy cores on 128 physical cores |
| PSI `cpu full` model | 0 below 1.0 utilization | E2: `full` stayed 0 through the measured range; the linear map above saturation is a projection |
| Red-line bands | enter 15%, exit 18% | E7: MemAvailable < 15% enter threshold; the higher recover threshold is flag-gated in the proposal |
| Sigma-caps invariant | 80% of node memory, platform baseline charged | E9: host-wide invariant including co-located reservations |
| Tighten margin / failure law | 512 MiB; margins below 512 MiB on a deeply occupied guest OOM | E8: `touched + 128 MB` OOM'd, `touched + 512 MB` held; the model encodes the measured law rather than re-deriving kernel reclaim |
| Balloon amount | `cap − (touched + margin)` | OSEP-0033 Test Plan's balloon-amount derivation |
| Dense snapshot size | 1:1 with the declared cap | E10: 536.9 MB per 512 MiB sandbox paused snapshot |

## Relation to the OSEP-0033 Test Plan

- The density sweep covers the Test Plan's E2E density and growth items: the 4000 × 512 MiB steady-state shape, the 600→4000 ramp with wall detection (where the first wall should be storage, not memory), and the elastic-pool invariant soak with a non-sandbox baseline charged against the same budget.
- `--scenario tighten-margins` covers the integration cases for margins, infeasible floors, and tighten ordering.
- `--scenario deflate-rescue` covers the burst survival case.
- Implementation-level acceptance numbers — decision RTT, balloon PATCH latency, red-line-to-tighten time, and the 24-hour misjudgment window — require a real fleet and the implementation under test. This harness does not fabricate them.

## Measurement boundaries

- The `sim` backend is a model. Its outputs are labeled as projections in the report header, the JSON summary, and the verdicts.
- The `host` backend samples signals; it does not drive workload and does not decide density tiers for you.
- Snapshot, pause/resume, restore churn, and balloon control-channel failures are outside this harness; storage appears only as a capacity axis.
- Simulated tier counts, PSI curves, and OOM counts depend on the calibration defaults. State any overrides alongside a result, or the numbers are not repeatable.

## Tests

```bash
cd tests/python
uv run pytest tests/test_density_tradeoff_benchmark.py
```

The offline suite covers model semantics, the policy layers, scenario expectations, host parsers against a fake proc tree, and CLI validation. It needs no hypervisor, cluster, or network.
