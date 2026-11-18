# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Offline coverage for the density tradeoff benchmark: model semantics,
policy layers, scenario expectations, host parsers, and CLI validation.
No hypervisor, no live node, no network."""

import json
from dataclasses import replace
from pathlib import Path

import pytest

from benchmarks import density_tradeoff as bench


def sim_config(**overrides):
    base = {
        "node_memory_mib": 16384.0,
        "platform_reserved_mib": 1024.0,
        "physical_cores": 8,
        "cap_mib": 512.0,
        "floor_mib": 256.0,
        "interval_s": 1.0,
        "duration_s": 30.0,
        "seed": 3,
        "create_rate": 0.0,
        "mix": {"idle": 1.0},
        "snapshot_mib_per_sandbox": 512.0,
        "storage_gib": 100.0,
        "storage_headroom_pct": 10.0,
        "postures": ("redline",),
        "scenario": "density",
    }
    base.update(overrides)
    return bench.SimConfig(**base)


def make_sandbox(cfg, kind="idle", offset=0.0, cap=None):
    profile = bench.resolve_profile(kind, cfg)
    return bench.SimSandbox(
        index=0,
        cap_mib=cap if cap is not None else cfg.cap_mib,
        profile=profile,
        offset=offset,
        anon=cfg.idle_anon_mib,
        file=cfg.image_file_mib,
    )


# --- input validation -------------------------------------------------------


def test_parse_mix_accepts_valid_and_rejects_invalid():
    assert bench.parse_mix("idle=0.5,sparse=0.5") == {"idle": 0.5, "sparse": 0.5}
    for bad in ("idle=0.5", "idle=3", "idle=0.5,nope=0.5", "idle=-1,sparse=2"):
        with pytest.raises(ValueError):
            bench.parse_mix(bad)


def test_arguments_reject_infeasible_configurations():
    with pytest.raises(SystemExit):
        bench.arguments(["--floor-mib", "512"])
    with pytest.raises(SystemExit):
        bench.arguments(["--mix", "idle=0.5"])
    with pytest.raises(SystemExit):
        bench.arguments(["--red-line-exit-pct", "10"])
    with pytest.raises(SystemExit):
        bench.arguments(["--backend", "host"])
    with pytest.raises(SystemExit):
        bench.arguments(
            ["--backend", "host", "--cgroup-root", "/proc", "--sandboxes", "4"]
        )
    with pytest.raises(SystemExit):
        bench.arguments(["--backend", "host", "--sandboxes", "-1"])
    # Zero is an explicit empty-fleet baseline, not an error.
    assert bench.arguments(["--backend", "host", "--sandboxes", "0"]).sandboxes == 0
    args = bench.arguments(["--tiers", "1,2"])
    assert args.ratios == [1.0, 2.0]
    assert args.mix["idle"] == 0.8


def test_floor_must_cover_the_modeled_boot_footprint():
    # A floor below the guest's idle anonymous + image-file pages would put a
    # born guest permanently over its effective cap.
    with pytest.raises(SystemExit):
        bench.arguments(["--floor-mib", "1"])
    assert bench.arguments(["--floor-mib", "30"]).floor_mib == 30.0
    with pytest.raises(ValueError):
        bench.run_tier(sim_config(floor_mib=1.0), "elastic", 1)


# --- model semantics --------------------------------------------------------


def test_idle_fleet_residency_matches_the_calibrated_baseline():
    cfg = sim_config()
    result = bench.run_tier(cfg, "redline", 12)
    summary = result.to_summary(cfg, "density")
    per_sandbox = summary["resident_mib"]["mean_per_sandbox"]
    # vmm_overhead + idle anon + image file pages ~= 26.2 MiB.
    assert 25.0 <= per_sandbox <= 27.5
    assert result.counters.redline_events == 0
    assert result.counters.guest_oom_kills == 0
    assert result.counters.host_oom_kills == 0
    assert summary["verdict"]["ok"] is True
    # Idle CPU: ~0.3 mcore per sandbox on 8 cores.
    assert summary["peak_busy_cores"] == pytest.approx(3.6 / 1000.0, abs=0.05)


def test_fpr_returns_freed_pages_after_the_measured_delay():
    cfg = sim_config(mix={"sparse": 1.0}, duration_s=40.0)
    sandbox = make_sandbox(cfg, kind="sparse", offset=5.0)
    samples = []
    for tick in range(40):
        sandbox.tick(tick, cfg)
        samples.append(sandbox.resident_mib(cfg))
    during_hold = samples[2]
    assert during_hold > 140.0  # 128 MiB working set plus baseline
    after_reclaim = samples[13]  # freed at tick 5, returned 6 s later
    assert during_hold - after_reclaim == pytest.approx(128.0, abs=2.0)


def test_dense_ramp_starts_at_the_guests_admission():
    cfg = sim_config(mix={"dense": 1.0}, cap_mib=512.0)
    late = make_sandbox(cfg, kind="dense", cap=512.0)
    late.birth_tick = 13  # admitted mid-run
    for tick in range(13, 16):
        late.tick(tick, cfg)
    # Two seconds after admission the guest holds two seconds of ramp
    # (2 + 100*2 MiB), not the fleet's 15 seconds of accumulated ramp.
    assert late.anon == pytest.approx(202.2, abs=2.0)
    early = make_sandbox(cfg, kind="dense", cap=512.0)
    early.tick(3, cfg)
    assert early.anon == pytest.approx(302.2, abs=2.0)


def test_deflate_on_oom_rescues_a_burst_beyond_the_effective_cap():
    cfg = sim_config(
        cap_mib=2048.0, floor_mib=1024.0, mix={"dense": 1.0}, fill_ratio=0.6
    )
    sandbox = make_sandbox(cfg, kind="dense", offset=0.0)
    sandbox.balloon = 1024.0  # elastic create: effective cap is the floor
    for tick in range(60):
        sandbox.tick(tick, cfg)
    assert sandbox.anon + sandbox.file > 1024.0  # burst exceeded the floor
    assert sandbox.deflate_saves > 0
    assert sandbox.oom_kills == 0


def test_burst_without_deflate_on_oom_kills_the_guest_workload():
    cfg = sim_config(
        cap_mib=2048.0,
        floor_mib=1024.0,
        mix={"dense": 1.0},
        fill_ratio=0.6,
        deflate_on_oom=False,
    )
    sandbox = make_sandbox(cfg, kind="dense", offset=0.0)
    sandbox.balloon = 1024.0
    for tick in range(60):
        sandbox.tick(tick, cfg)
    assert sandbox.oom_kills >= 1
    assert sandbox.workload_dead is True


def test_deflation_never_exceeds_the_declared_cap():
    # A burst larger than the balloon can cover must OOM the workload, not
    # push the balloon negative and grant capacity above the declared cap.
    cfg = sim_config(cap_mib=256.0, floor_mib=128.0, mix={"bursty": 1.0})
    sandbox = make_sandbox(cfg, kind="bursty", offset=0.0)
    sandbox.balloon = 128.0
    for tick in range(30):
        sandbox.tick(tick, cfg)
        assert sandbox.balloon >= 0.0
        assert sandbox.effective_mib <= sandbox.cap_mib
    assert sandbox.balloon == 0.0
    assert sandbox.oom_kills >= 1
    # The partial release did not save the workload, so it is not a save.
    assert sandbox.deflate_saves == 0


def test_tighten_plan_uses_cap_minus_floor_derivation():
    cfg = sim_config(cap_mib=2048.0)
    sandbox = make_sandbox(cfg, kind="idle")
    sandbox.anon = 1000.0
    sandbox.file = 0.0
    counters = bench.TierCounters()
    amount = bench.plan_tighten(sandbox, 10, cfg, 512.0, counters)
    assert amount == pytest.approx(2048.0 - (1000.0 + 512.0))
    assert counters.tighten_actions == 1
    assert counters.tighten_skips == 0


def test_thin_margin_on_deep_occupancy_keeps_the_known_failure_visible():
    cfg = sim_config(cap_mib=2048.0)
    sandbox = make_sandbox(cfg, kind="idle")
    sandbox.anon = 1500.0
    sandbox.file = 0.0
    counters = bench.TierCounters()
    assert bench.plan_tighten(sandbox, 10, cfg, 128.0, counters) is None
    assert sandbox.oom_kills == 1
    assert counters.tighten_actions == 0


def test_inflate_first_ooms_a_still_allocating_guest():
    counters = bench.TierCounters()
    unordered = sim_config(cap_mib=2048.0, throttle_first=False)
    sandbox = make_sandbox(unordered, kind="dense", offset=0.0)
    sandbox.anon = 250.0
    sandbox.file = 0.0
    assert bench.plan_tighten(sandbox, 3, unordered, 512.0, counters) is None
    assert sandbox.oom_kills == 1
    ordered_cfg = sim_config(cap_mib=2048.0)
    ordered = make_sandbox(ordered_cfg, kind="dense", offset=0.0)
    ordered.anon = 250.0
    ordered.file = 0.0
    ordered.throttled = True
    assert bench.plan_tighten(ordered, 3, ordered_cfg, 512.0, counters) is not None
    assert ordered.oom_kills == 0


def test_tighten_plan_skips_infeasible_floor():
    cfg = sim_config(cap_mib=512.0)
    sandbox = make_sandbox(cfg, kind="idle")
    counters = bench.TierCounters()
    assert bench.plan_tighten(sandbox, 10, cfg, 512.0, counters) is None
    assert counters.tighten_skips == 1
    assert counters.tighten_actions == 0
    assert sandbox.oom_kills == 0


def test_tighten_plan_never_deflates_a_larger_elastic_balloon():
    cfg = sim_config(cap_mib=2048.0)
    sandbox = make_sandbox(cfg, kind="idle")
    sandbox.balloon = 1792.0  # elastic create: cap minus a 256 MiB floor
    counters = bench.TierCounters()
    # The plan would be 1512.8 MiB, below the current balloon: no action, and
    # the controller must leave the balloon and the throttle intact.
    assert bench.plan_tighten(sandbox, 10, cfg, 512.0, counters) is None
    assert counters.tighten_skips == 1
    assert sandbox.balloon == 1792.0


# --- policy layers ----------------------------------------------------------


def test_sigma_caps_invariant_charges_the_platform_baseline():
    cfg = sim_config(node_memory_mib=8192.0, platform_reserved_mib=1024.0)
    budget = bench.invariant_limit_mib(cfg)
    assert budget == pytest.approx(0.8 * 8192.0 - 1024.0)
    result = bench.run_tier(cfg, "elastic", 20)
    assert result.counters.granted == 10  # floor(5531.2 / 512)
    assert result.placed_sandboxes == 10
    assert result.counters.invariant_denied == 10
    assert result.placed_sandboxes * cfg.cap_mib <= budget


def test_red_line_freezes_creates_and_the_floor_guarantee_releases_them():
    cfg = sim_config(
        create_rate=2.0,
        force_redline_ticks=(5,),
        release_redline_tick=15,
    )
    frozen = bench.run_tier(cfg, "redline", 30)
    assert frozen.counters.redline_events == 1
    # With the floor guarantee, creates during the freeze are floor grants,
    # never unchecked admissions.
    assert frozen.counters.redline_denied == 0
    assert frozen.counters.floor_grants > 0
    without_guarantee = bench.run_tier(
        replace(cfg, floor_guarantee=False), "redline", 30
    )
    assert without_guarantee.counters.floor_grants == 0
    assert without_guarantee.counters.redline_denied > 0


def test_red_line_reacts_before_last_resort_eviction():
    # One dense tick can step over both the enter band and the eviction
    # reserve on a small node; the crossing must engage the freeze before
    # last-resort eviction runs, so the policy is never bypassed.
    cfg = sim_config(
        node_memory_mib=1024.0,
        platform_reserved_mib=0.0,
        mix={"dense": 1.0},
        duration_s=30.0,
    )
    result = bench.run_tier(cfg, "redline", 2)
    assert result.counters.redline_events >= 1


def test_forced_red_line_holds_until_its_release_tick():
    # The synthetic trigger freezes for its whole forced window even on a
    # relaxed node; a one-tick freeze would never exercise the documented
    # 20-second window.
    rows = bench.run_tighten_matrix(sim_config())
    ordered = [row for row in rows if row.throttle_first]
    assert ordered
    for row in ordered:
        assert row.result.counters.ticks_redline == 20


def test_floor_granted_guests_join_the_active_freeze():
    # A guest admitted while the red line is already active must be frozen
    # too: otherwise the tighten pass never sees it and deflate_on_oom grows
    # it toward its cap, raising host charge until evictions fire.
    cfg = sim_config(
        node_memory_mib=8192.0,
        platform_reserved_mib=1024.0,
        create_rate=2.0,
        mix={"dense": 1.0},
        fill_ratio=0.5,
        duration_s=40.0,
    )
    result = bench.run_tier(cfg, "redline", 40)
    assert result.counters.redline_events == 1
    assert result.counters.floor_grants > 0
    assert result.counters.host_oom_kills == 0
    # While the freeze holds, every guest is stopped: consecutive active
    # samples with an unchanged fleet size show an unchanged residency.
    active = [sample for sample in result.samples if sample["redline_active"]]
    pairs = list(zip(active, active[1:], strict=False))
    frozen_pairs = [
        pair for pair in pairs if pair[0]["sandboxes"] == pair[1]["sandboxes"]
    ]
    assert frozen_pairs
    for before, after in frozen_pairs:
        assert after["resident_mib"] == pytest.approx(before["resident_mib"])


def test_drain_resumes_only_with_projected_headroom():
    # 3x oversell on a 1 GiB node: full resumption would dive straight
    # through the enter band into the eviction reserve, and last-resort
    # eviction runs before the next re-entry check can freeze the fleet. The
    # drain must therefore restore only what the node can absorb.
    cfg = sim_config(
        node_memory_mib=1024.0,
        platform_reserved_mib=0.0,
        mix={"sparse": 0.5, "dense": 0.5},
        interval_s=0.25,
        duration_s=120.0,
    )
    result = bench.run_tier(cfg, "redline", 6)
    assert result.counters.redline_events >= 1
    assert result.counters.host_oom_kills == 0
    assert result.counters.restored >= 1


def test_red_line_recovers_when_headroom_returns():
    cfg = sim_config(
        mix={"dense": 1.0},
        cap_mib=2048.0,
        duration_s=120.0,
        force_redline_ticks=(10,),
        release_redline_tick=30,
        tighten_margin_mib=128.0,
    )
    result = bench.run_tier(cfg, "redline", 1)
    assert result.counters.redline_events == 1
    assert result.counters.restored >= 1
    assert result.redline_unrecovered is False
    assert result.samples[-1]["redline_active"] is False


def test_red_line_re_enters_when_draining_headroom_collapses():
    cfg = sim_config(
        cap_mib=512.0,
        duration_s=30.0,
        restore_batch=1,
        force_redline_ticks=(10, 14),
        release_redline_tick=12,
    )
    # The second trigger lands while the first drain is still restoring its
    # batch: re-entry must abort the drain instead of suppressing it.
    result = bench.run_tier(cfg, "redline", 5)
    assert result.counters.redline_events == 2


def test_redline_floor_grants_are_restored_after_recovery():
    cfg = sim_config(
        create_rate=1.0,
        duration_s=20.0,
        force_redline_ticks=(2,),
        release_redline_tick=8,
    )
    result = bench.run_tier(cfg, "redline", 6)
    assert result.counters.floor_grants > 0
    assert result.samples[-1]["balloon_mib"] == 0.0


def test_mid_drain_re_entry_preserves_original_balloons():
    cfg = sim_config(
        cap_mib=2048.0,
        fill_ratio=0.7,
        duration_s=30.0,
        restore_batch=1,
        force_redline_ticks=(10, 14),
        release_redline_tick=12,
    )
    result = bench.run_tier(cfg, "redline", 2)
    assert result.counters.redline_events == 2
    # A guest still tightened from the aborted cycle keeps its original
    # pre-cycle balloon, so the next recovery deflates it fully.
    assert result.samples[-1]["balloon_mib"] == 0.0


def test_host_oom_reserve_default_stays_below_the_enter_band():
    for node in (1024.0, 2048.0, 4096.0, 16384.0, 515745.0):
        cfg = sim_config(node_memory_mib=node)
        assert bench.host_oom_reserve(cfg) < cfg.red_line_enter_pct / 100.0 * node


def test_host_oom_reserve_scales_to_the_configured_node():
    cfg = sim_config(
        node_memory_mib=4096.0,
        platform_reserved_mib=1024.0,
        mix={"dense": 1.0},
        cap_mib=512.0,
        duration_s=60.0,
    )
    assert bench.host_oom_reserve(cfg) == pytest.approx(256.0)
    result = bench.run_tier(cfg, "redline", 8)
    # Dense fill crosses the red-line band before the scaled reserve, so the
    # policy freezes guests instead of evicting them.
    assert result.counters.redline_events >= 1
    assert result.counters.host_oom_kills == 0


def test_red_line_restore_keeps_elastic_floor_balloons():
    # An elastic guest already tighter than the tighten target must keep its
    # floor balloon across a red-line cycle instead of being deflated to 0.
    cfg = sim_config(
        cap_mib=2048.0,
        floor_mib=256.0,
        duration_s=40.0,
        force_redline_ticks=(10,),
        release_redline_tick=20,
    )
    result = bench.run_tier(cfg, "elastic", 1)
    assert result.counters.redline_events == 1
    balloon = [sample["balloon_mib"] for sample in result.samples]
    assert balloon[0] == pytest.approx(1792.0)
    assert all(value == pytest.approx(1792.0) for value in balloon)


def test_unrecovered_red_line_is_a_verdict_violation():
    # Dense fill pushes the node below the red line; tighten floors are
    # infeasible at this cap, so the freeze never restores headroom.
    cfg = sim_config(
        node_memory_mib=4096.0,
        platform_reserved_mib=256.0,
        mix={"dense": 1.0},
        cap_mib=512.0,
        duration_s=60.0,
        host_oom_reserve_mib=64.0,
    )
    result = bench.run_tier(cfg, "redline", 8)
    assert result.counters.redline_events >= 1
    assert result.counters.tighten_skips > 0
    assert result.redline_unrecovered is True
    summary = result.to_summary(cfg, "density")
    assert "red_line_unrecovered" in summary["verdict"]["violations"]


def test_per_sandbox_residency_averages_over_live_guests_only():
    # A ramped fleet must not dilute the per-sandbox cost with empty ticks.
    cfg = sim_config(create_rate=1.0, duration_s=20.0)
    result = bench.run_tier(cfg, "redline", 16)
    summary = result.to_summary(cfg, "density")
    assert result.placed_sandboxes == 16
    assert summary["resident_mib"]["mean_per_sandbox"] == pytest.approx(26.2, abs=0.5)


def test_host_oom_evicts_instead_of_exhausting_the_node():
    cfg = sim_config(
        node_memory_mib=4096.0,
        platform_reserved_mib=1024.0,
        mix={"dense": 1.0},
        cap_mib=512.0,
        duration_s=60.0,
        red_line_enter_pct=1.0,
        red_line_exit_pct=2.0,
        host_oom_reserve_mib=64.0,
    )
    result = bench.run_tier(cfg, "redline", 8)
    assert result.counters.host_oom_kills >= 1
    assert result.mem_available_min_pct > 0.0
    summary = result.to_summary(cfg, "density")
    assert "host_oom_kills" in summary["verdict"]["violations"]


def test_evicted_fleets_still_report_their_per_sandbox_cost():
    # Every guest is host-evicted before the run ends; the per-sandbox cost
    # measured while they were alive must survive serialization instead of
    # collapsing to None (rendered as zero).
    cfg = sim_config(
        node_memory_mib=1024.0,
        platform_reserved_mib=0.0,
        mix={"dense": 1.0},
        fill_ratio=0.7,
        duration_s=12.0,
        host_oom_reserve_mib=900.0,
    )
    result = bench.run_tier(cfg, "redline", 4)
    assert result.placed_sandboxes == 0
    assert result.counters.host_oom_kills == 4
    summary = result.to_summary(cfg, "density")
    assert summary["resident_mib"]["mean_per_sandbox"] is not None
    assert summary["resident_mib"]["mean_per_sandbox"] > 0
    row = bench.density_table_row(cfg, result)
    assert "n/a" not in row


def test_storage_wall_is_reported():
    cfg = sim_config(storage_gib=1.0, storage_headroom_pct=10.0)
    result = bench.run_tier(cfg, "redline", 12)
    summary = result.to_summary(cfg, "density")
    assert summary["storage_gib"]["total"] == pytest.approx(12 * 512 / 1024.0)
    assert "storage_over_limit" in summary["verdict"]["violations"]


# --- scenarios --------------------------------------------------------------


def test_density_scenario_runs_every_tier_and_posture():
    cfg = sim_config(postures=("redline", "elastic"))
    results = bench.run_density_scenario(cfg, [0.5, 1.0])
    assert len(results) == 4
    report = bench.render_density_report(cfg, results)
    assert "Posture: redline" in report
    assert "Posture: elastic" in report
    assert "| 0.50x/0.50x |" in report


def test_tighten_matrix_matches_test_plan_expectations():
    cfg = sim_config()
    rows = bench.run_tighten_matrix(cfg)
    assert len(rows) == 5
    assert all(row.to_row()["verdict"]["ok"] for row in rows)
    # Each row serializes with its own cap and mix, not the top-level ones.
    row_2048 = next(row for row in rows if row.cap_mib == 2048.0)
    assert row_2048.to_row()["summary"]["cap_mib"] == 2048.0
    assert row_2048.to_row()["summary"]["mix"] == {"dense": 1.0}
    by_case = {row.case: row for row in rows}
    assert by_case["safe margin, ordered"].result.counters.guest_oom_kills == 0
    assert by_case["known-bad margin, ordered"].result.counters.guest_oom_kills > 0
    assert by_case["infeasible floor, ordered"].result.counters.tighten_skips > 0
    assert by_case["safe margin, inflate-first"].result.counters.guest_oom_kills > 0


def test_tighten_matrix_pins_forced_events_to_elapsed_time():
    # The forced window is a wall-clock experiment: a finer --interval moves
    # the trigger to the same elapsed second (25 s), not to tick 25, and a
    # horizon shorter than the case needs is extended to cover it.
    fine = bench.run_tighten_matrix(sim_config(interval_s=0.1, duration_s=30.0))
    by_case = {row.case: row for row in fine}
    assert by_case["known-bad margin, ordered"].force_tick == 250
    assert by_case["safe margin, inflate-first"].force_tick == 100
    assert all(row.to_row()["verdict"]["ok"] for row in fine)
    coarse = bench.run_tighten_matrix(sim_config(interval_s=1.0, duration_s=5.0))
    ordered = [row for row in coarse if "inflate-first" not in row.case]
    inflate_first = next(row for row in coarse if "inflate-first" in row.case)
    assert all(row.force_tick == 25 for row in ordered)
    assert inflate_first.force_tick == 10
    assert all(
        row.to_row()["summary"]["horizon"]["duration_s"] >= 50.0 for row in ordered
    )
    assert inflate_first.to_row()["summary"]["horizon"]["duration_s"] >= 35.0


def test_tighten_ordering_uses_guest_age_for_late_arrivals():
    # The inflate-first OOM law fires only for a guest that is genuinely
    # still allocating in the next interval; a guest admitted one tick ago
    # (inside its own ramp stagger) is not.
    cfg = sim_config(cap_mib=2048.0, mix={"dense": 1.0}, throttle_first=False)
    late = make_sandbox(cfg, kind="dense", offset=5.0, cap=2048.0)
    late.birth_tick = 100
    counters = bench.TierCounters()
    plan = bench.plan_tighten(late, 100, cfg, 512.0, counters)
    assert plan is not None
    assert late.oom_kills == 0
    assert counters.tighten_actions == 1


def test_deflate_rescue_extends_a_short_horizon_to_reach_the_burst():
    # A 1 s horizon cannot ramp the guest to its effective cap; the scenario
    # extends it so the advertised deflate comparison is actually exercised,
    # in whole ticks so even a coarse cadence samples the burst.
    rescue_cfg, rows, _ = bench.run_deflate_rescue(sim_config(duration_s=1.0))
    assert rescue_cfg.duration_s >= 20.0
    assert all(row["verdict"]["ok"] for row in rows)
    coarse_cfg, coarse_rows, _ = bench.run_deflate_rescue(
        sim_config(duration_s=1.0, interval_s=8.0)
    )
    assert coarse_cfg.duration_s >= 48.0
    assert all(row["verdict"]["ok"] for row in coarse_rows)


def test_deflate_rescue_verdict_rejects_host_evicted_guests():
    # A guest that the host evicted absorbed nothing; the outer row verdict
    # must fail even though no guest OOM was recorded.
    rescue_cfg, rows, _ = bench.run_deflate_rescue(
        sim_config(
            node_memory_mib=4096.0,
            platform_reserved_mib=0.0,
            host_oom_reserve_mib=3000.0,
        )
    )
    rescued = next(row for row in rows if row["deflate_on_oom"])
    assert rescued["sandboxes"] == 0
    assert rescued["host_oom_kills"] == 1
    assert rescued["verdict"]["ok"] is False


def test_deflate_rescue_scenario_matches_expectations():
    rescue_cfg, rows, results = bench.run_deflate_rescue(sim_config())
    assert rescue_cfg.cap_mib == 2048.0
    assert rescue_cfg.mix == {"dense": 1.0}
    assert len(rows) == len(results) == 2
    assert all(row["verdict"]["ok"] for row in rows)
    assert rows[0]["deflate_on_oom"] is True
    assert rows[0]["deflate_saves"] > 0
    assert rows[1]["guest_oom_kills"] > 0


def test_simulation_is_deterministic_for_a_fixed_seed():
    cfg = sim_config()
    first = bench.run_tier(cfg, "redline", 20).to_summary(cfg, "density")
    second = bench.run_tier(cfg, "redline", 20).to_summary(cfg, "density")
    assert first == second


# --- host backend -----------------------------------------------------------


def write_fake_proc(root):
    (root / "pressure").mkdir()
    (root / "meminfo").write_text(
        "MemTotal:       1056896 kB\nMemAvailable:    800000 kB\n", encoding="utf-8"
    )
    (root / "pressure" / "cpu").write_text(
        "some avg10=1.00 avg60=2.50 avg300=3.00\nfull avg10=0.00 avg60=0.50 avg300=0.60\n",
        encoding="utf-8",
    )
    (root / "pressure" / "memory").write_text(
        "some avg10=0.10 avg60=0.20 avg300=0.30\nfull avg10=0.00 avg60=0.00 avg300=0.00\n",
        encoding="utf-8",
    )
    (root / "vmstat").write_text("nr_free_pages 1\noom_kill 42\n", encoding="utf-8")


def write_fake_cgroups(root):
    first = root / "sandbox-a"
    first.mkdir()
    (first / "memory.current").write_text("123456789\n", encoding="utf-8")
    (first / "memory.max").write_text("536870912\n", encoding="utf-8")
    (first / "memory.events").write_text("oom 0\noom_kill 2\n", encoding="utf-8")
    second = root / "sandbox-b"
    second.mkdir()
    (second / "memory.current").write_text("234567890\n", encoding="utf-8")
    (second / "memory.max").write_text("max\n", encoding="utf-8")
    (second / "memory.events").write_text("oom_kill 1\n", encoding="utf-8")


def test_host_parsers_read_proc_and_cgroup_accounting(tmp_path):
    proc = tmp_path / "proc"
    proc.mkdir()
    write_fake_proc(proc)
    cgroups = tmp_path / "cgroups"
    cgroups.mkdir()
    write_fake_cgroups(cgroups)
    meminfo = bench.read_meminfo(proc)
    assert meminfo["mem_total_mib"] == pytest.approx(1056896 / 1024.0)
    pressure = bench.read_pressure(proc / "pressure" / "cpu")
    assert pressure["some_avg60"] == 2.5
    assert pressure["full_avg60"] == 0.5
    assert bench.read_vmstat_oom_kills(proc) == 42
    dirs = bench.list_cgroup_dirs(cgroups)
    assert [path.name for path in dirs] == ["sandbox-a", "sandbox-b"]
    sample = bench.read_cgroup_sample(dirs[0])
    assert sample["current_mib"] == pytest.approx(123456789 / (1024.0 * 1024.0))
    assert sample["max_mib"] == pytest.approx(512.0)
    assert sample["oom_kill"] == 2
    unlimited = bench.read_cgroup_sample(dirs[1])
    assert unlimited["max_mib"] is None
    # The directory inode is the cgroup's generation identity.
    assert sample["inode"] == dirs[0].stat().st_ino


def test_host_summary_aggregates_samples(tmp_path):
    proc = tmp_path / "proc"
    proc.mkdir()
    write_fake_proc(proc)
    cgroups = tmp_path / "cgroups"
    cgroups.mkdir()
    write_fake_cgroups(cgroups)
    cfg = bench.HostConfig(
        proc_root=proc,
        cgroup_root=cgroups,
        duration_s=0.2,
        interval_s=0.05,
        cap_mib=512.0,
        sandboxes_override=None,
    )
    samples = bench.collect_host_samples(cfg)
    assert len(samples) >= 2
    assert samples[0]["cgroups"] == 2
    summary = bench.summarize_host_samples(samples, cfg)
    assert summary["sandboxes"] == 2
    assert summary["events"]["host_oom_kills_delta"] == 0
    assert summary["resident_mib"]["mean_per_sandbox"] == pytest.approx(170.7, abs=0.5)
    assert summary["psi_max_pct"]["mem_some"] == pytest.approx(0.2)
    assert summary["verdict"]["ok"] is True
    report = bench.render_host_report(summary, cfg)
    assert "backend: host" in report


def test_host_summary_flags_oom_deltas(tmp_path):
    proc = tmp_path / "proc"
    proc.mkdir()
    write_fake_proc(proc)
    cfg = bench.HostConfig(
        proc_root=proc,
        cgroup_root=None,
        duration_s=0.1,
        interval_s=0.05,
        cap_mib=512.0,
        sandboxes_override=4,
    )
    samples = bench.collect_host_samples(cfg)
    samples[0]["vmstat_oom_kill"] = 10
    samples[-1]["vmstat_oom_kill"] = 12
    summary = bench.summarize_host_samples(samples, cfg)
    assert summary["events"]["host_oom_kills_delta"] == 2
    assert "oom_kills" in summary["verdict"]["violations"]


def test_host_summary_without_cgroups_reports_residency_unavailable(tmp_path):
    proc = tmp_path / "proc"
    proc.mkdir()
    write_fake_proc(proc)
    cfg = bench.HostConfig(
        proc_root=proc,
        cgroup_root=None,
        duration_s=0.1,
        interval_s=0.05,
        cap_mib=512.0,
        sandboxes_override=4,
    )
    samples = bench.collect_host_samples(cfg)
    assert all(sample["cgroup_current_mib"] is None for sample in samples)
    summary = bench.summarize_host_samples(samples, cfg)
    assert summary["resident_mib"]["mean_total"] is None
    assert summary["resident_mib"]["mean_per_sandbox"] is None
    assert summary["events"]["cgroup_oom_kills_delta"] is None
    report = bench.render_host_report(summary, cfg)
    assert "| n/a |" in report


def test_read_cgroups_tolerates_a_vanished_sandbox(tmp_path, monkeypatch):
    cgroups = tmp_path / "cgroups"
    cgroups.mkdir()
    write_fake_cgroups(cgroups)
    original = bench.read_cgroup_sample

    def flaky(path):
        if path.name == "sandbox-a":
            raise FileNotFoundError(path)
        return original(path)

    monkeypatch.setattr(bench, "read_cgroup_sample", flaky)
    samples = bench.read_cgroups(cgroups)
    assert list(samples) == [str(cgroups / "sandbox-b")]


def test_accumulate_cgroup_oom_counts_kills_across_churn():
    state, seen = {}, set()
    # First-snapshot paths are baselines, not deltas.
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (10, 1), "b": (11, 0)}, baseline_new_paths=True
        )
        == 0
    )
    # a accumulates; b disappears with its counter already counted.
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (10, 4)}, baseline_new_paths=False
        )
        == 3
    )
    # A path that returns after vanishing counts from zero again.
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (10, 4), "b": (12, 2)}, baseline_new_paths=False
        )
        == 2
    )
    # A cgroup created during the run counts its first-sighting kills.
    assert (
        bench.accumulate_cgroup_oom(
            state,
            seen,
            {"a": (10, 4), "b": (12, 2), "c": (13, 3)},
            baseline_new_paths=False,
        )
        == 3
    )
    # A counter that decreases was reset in place: count it from zero,
    # never negative.
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (10, 1)}, baseline_new_paths=False
        )
        == 1
    )
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (10, 1)}, baseline_new_paths=False
        )
        == 0
    )


def test_accumulate_cgroup_oom_detects_recreation_at_the_same_counter():
    state, seen = {}, set()
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (10, 1)}, baseline_new_paths=False
        )
        == 1
    )
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (10, 1)}, baseline_new_paths=False
        )
        == 0
    )
    # Deleted and recreated between polls: the replacement's counter equals
    # the old value, so only the changed directory inode reveals the kill.
    assert (
        bench.accumulate_cgroup_oom(
            state, seen, {"a": (20, 1)}, baseline_new_paths=False
        )
        == 1
    )


def test_cli_host_run_writes_array_summary(tmp_path, capsys):
    proc = tmp_path / "proc"
    proc.mkdir()
    write_fake_proc(proc)
    cgroups = tmp_path / "cgroups"
    cgroups.mkdir()
    write_fake_cgroups(cgroups)
    out_summary = tmp_path / "host-summary.json"
    exit_code = bench.main(
        [
            "--backend",
            "host",
            "--proc-root",
            str(proc),
            "--cgroup-root",
            str(cgroups),
            "--duration",
            "0.1",
            "--interval",
            "0.05",
            "--out-summary",
            str(out_summary),
        ]
    )
    assert exit_code == 0
    assert "backend: host" in capsys.readouterr().out
    data = json.loads(out_summary.read_text(encoding="utf-8"))
    assert isinstance(data, list)
    assert data[0]["backend"] == "host"
    assert data[0]["sandboxes"] == 2


# --- CLI end to end (sim, hermetic) -----------------------------------------


def test_host_summary_separates_contained_from_node_level_kills():
    base = {
        "mem_available_mib": 900.0,
        "mem_total_mib": 1000.0,
        "psi_cpu_some_avg60": 0.0,
        "psi_cpu_full_avg60": 0.0,
        "psi_mem_some_avg60": 0.0,
        "cgroups": 2,
        "cgroup_current_mib": 300.0,
    }
    samples = [
        {**base, "vmstat_oom_kill": 10, "cgroup_oom_kill_accumulated": 0},
        {**base, "vmstat_oom_kill": 14, "cgroup_oom_kill_accumulated": 3},
    ]
    cfg = bench.HostConfig(
        proc_root=Path("/nonexistent"),
        cgroup_root=None,
        duration_s=2.0,
        interval_s=1.0,
        cap_mib=512.0,
        sandboxes_override=None,
    )
    summary = bench.summarize_host_samples(samples, cfg)
    assert summary["events"]["system_oom_kills_delta"] == 4
    assert summary["events"]["cgroup_oom_kills_delta"] == 3
    assert summary["events"]["host_oom_kills_delta"] == 1


def test_effective_oom_reserve_is_recorded_in_outputs():
    cfg = sim_config(node_memory_mib=4096.0)
    assert "host OOM reserve 256 MiB" in bench.policy_line(cfg)
    result = bench.run_tier(cfg, "redline", 1)
    summary = result.to_summary(cfg, "density")
    assert summary["host_oom_reserve_mib"] == pytest.approx(256.0)


def test_summary_serializes_the_effective_configuration():
    cfg = sim_config(
        node_memory_mib=8192.0,
        invariant_pct=50.0,
        red_line_enter_pct=20.0,
        tighten_margin_mib=128.0,
        oom_margin_threshold_mib=256.0,
        storage_gib=42.0,
    )
    summary = bench.run_tier(cfg, "elastic", 4).to_summary(cfg, "density")
    config = summary["config"]
    assert config["node_memory_mib"] == 8192.0
    assert config["invariant_pct"] == 50.0
    assert config["red_line_enter_pct"] == 20.0
    assert config["tighten_margin_mib"] == 128.0
    assert config["oom_margin_threshold_mib"] == 256.0
    assert config["oom_occupancy_ratio"] == 0.5
    assert config["throttle_first"] is True
    assert config["deflate_on_oom"] is True
    assert config["storage_gib"] == 42.0


def test_host_summary_fails_on_cgroup_only_oom_delta():
    # vmstat is read before the cgroup files: a kill landing in that window
    # appears only in the fleet delta and must still fail the verdict.
    base = {
        "mem_available_mib": 900.0,
        "mem_total_mib": 1000.0,
        "psi_cpu_some_avg60": 0.0,
        "psi_cpu_full_avg60": 0.0,
        "psi_mem_some_avg60": 0.0,
        "vmstat_oom_kill": 0,
        "cgroups": 2,
        "cgroup_current_mib": 300.0,
    }
    samples = [
        {**base, "cgroup_oom_kill_accumulated": 0},
        {**base, "cgroup_oom_kill_accumulated": 1},
    ]
    cfg = bench.HostConfig(
        proc_root=Path("/nonexistent"),
        cgroup_root=None,
        duration_s=2.0,
        interval_s=1.0,
        cap_mib=512.0,
        sandboxes_override=None,
    )
    summary = bench.summarize_host_samples(samples, cfg)
    assert summary["events"]["system_oom_kills_delta"] == 0
    assert summary["events"]["cgroup_oom_kills_delta"] == 1
    assert "oom_kills" in summary["verdict"]["violations"]


def test_arguments_reject_impossible_node_shapes():
    with pytest.raises(SystemExit):
        # The default reserve is larger than the overridden node.
        bench.arguments(["--node-memory-mib", "16384"])
    with pytest.raises(SystemExit):
        bench.arguments(
            ["--node-memory-mib", "16384", "--platform-reserved-mib", "16384"]
        )
    with pytest.raises(SystemExit):
        # The matrix's earliest forced trigger cannot be resolved at this
        # cadence.
        bench.arguments(["--scenario", "tighten-margins", "--interval", "30"])
    with pytest.raises(ValueError):
        bench.run_tighten_matrix(sim_config(interval_s=30.0))


def test_host_backend_accepts_cap_only_without_floor():
    args = bench.arguments(
        ["--backend", "host", "--sandboxes", "10", "--cap-mib", "256"]
    )
    assert args.cap_mib == 256.0


def test_host_per_sandbox_residency_uses_each_samples_live_count():
    base = {
        "mem_available_mib": 900.0,
        "mem_total_mib": 1000.0,
        "psi_cpu_some_avg60": 0.0,
        "psi_cpu_full_avg60": 0.0,
        "psi_mem_some_avg60": 0.0,
        "vmstat_oom_kill": 0,
        "cgroup_oom_kill": 0,
        "cgroup_oom_kill_accumulated": 0,
    }
    samples = [
        {**base, "cgroups": 1, "cgroup_current_mib": 100.0},
        {**base, "cgroups": 2, "cgroup_current_mib": 200.0},
    ]
    cfg = bench.HostConfig(
        proc_root=Path("/nonexistent"),
        cgroup_root=None,
        duration_s=2.0,
        interval_s=1.0,
        cap_mib=512.0,
        sandboxes_override=None,
    )
    summary = bench.summarize_host_samples(samples, cfg)
    assert summary["sandboxes"] == 2
    # (100/1 + 200/2) / 2, not (100 + 200) / 2 / 2.
    assert summary["resident_mib"]["mean_per_sandbox"] == pytest.approx(100.0)


def test_cli_matrix_jsonl_carries_tick_samples(tmp_path):
    out_jsonl = tmp_path / "matrix.jsonl"
    assert (
        bench.main(["--scenario", "deflate-rescue", "--out-jsonl", str(out_jsonl)]) == 0
    )
    types = [json.loads(line)["type"] for line in out_jsonl.read_text().splitlines()]
    assert "sample" in types
    assert "matrix-row" in types


def test_cli_matrix_reports_describe_the_executed_configuration(capsys):
    assert bench.main(["--scenario", "deflate-rescue"]) == 0
    deflate_report = capsys.readouterr().out
    assert "cap 2048 MiB, floor 1024 MiB" in deflate_report
    assert "dense=1.00" in deflate_report
    assert bench.main(["--scenario", "tighten-margins"]) == 0
    tighten_report = capsys.readouterr().out
    assert (
        "rows override cap, tighten margin, ordering, mix, fill ratio" in tighten_report
    )
    assert "host OOM reserve 64 MiB" in tighten_report
    assert "- horizon: 300 s at 1 s intervals" in tighten_report
    # A too-short user horizon must not leak into the header: the rows
    # extended their horizon to contain the forced window.
    assert bench.main(["--scenario", "tighten-margins", "--duration", "5"]) == 0
    short_report = capsys.readouterr().out
    assert "- horizon: per row (35 s, 50 s) at 1 s intervals" in short_report
    assert "- horizon: 5 s" not in short_report


def test_fractional_horizon_is_preserved_in_metadata():
    args = bench.arguments(["--duration", "0.5", "--interval", "0.1"])
    cfg = bench.build_sim_config(args)
    assert "0.5 s at 0.1 s intervals" in bench.horizon_line(cfg)
    result = bench.run_tier(cfg, "redline", 1)
    horizon = result.to_summary(cfg, "density")["horizon"]
    assert horizon["duration_s"] == 0.5
    assert horizon["interval_s"] == 0.1


def test_cli_density_run_writes_all_outputs(tmp_path, capsys):
    out_jsonl = tmp_path / "samples.jsonl"
    out_summary = tmp_path / "summary.json"
    report = tmp_path / "report.md"
    exit_code = bench.main(
        [
            "--tiers",
            "1",
            "--posture",
            "redline",
            "--duration",
            "20",
            "--out-jsonl",
            str(out_jsonl),
            "--out-summary",
            str(out_summary),
            "--report",
            str(report),
        ]
    )
    assert exit_code == 0
    assert "Density tradeoff report" in capsys.readouterr().out
    assert "Density tradeoff report" in report.read_text(encoding="utf-8")
    lines = out_jsonl.read_text(encoding="utf-8").splitlines()
    assert any('"type": "summary"' in line for line in lines)
    assert any('"type": "sample"' in line for line in lines)
    assert out_summary.read_text(encoding="utf-8").startswith("[")
