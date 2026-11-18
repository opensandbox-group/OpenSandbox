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

"""Memory density tradeoff benchmark for Firecracker-scale sandbox fleets.

Sweeps density tiers — an oversell ratio is the sum of declared sandbox
memory caps divided by node memory — and reports the operating cost of each
tier: resident memory, PSI pressure, MemAvailable headroom, admission
denials, red-line activity, and OOM/eviction events.

Two backends:

- ``sim`` (default): a deterministic, calibrated model of a sandbox fleet.
  It encodes the density evidence published with OSEP-0033 (idle residency,
  free-page-reporting reclaim latency, balloon tighten ordering,
  ``deflate_on_oom`` isolation, the red-line state machine) as explicit,
  documented parameters and emits modeled projections. It needs no
  hypervisor, so CI and laptops can run every scenario.
- ``host``: a read-only sampler for a live Linux node. While an externally
  driven fleet runs, it records /proc/meminfo, /proc/pressure, and cgroup v2
  memory accounting, and emits the same cost table from measurements. It
  never creates, configures, or destroys sandboxes.

This harness is the reproducible evidence companion for the density and
oversubscription proposal (OSEP-0033,
https://github.com/opensandbox-group/OpenSandbox/pull/2915) and its Test
Plan. Simulated tiers are projections, not measurements; acceptance runs
still require a real fleet.

No third-party dependencies; Python 3.10+.
"""

import argparse
import json
import math
import random
import sys
import time
from collections import deque
from dataclasses import dataclass, field, replace
from pathlib import Path

OSEP_0033_URL = "https://github.com/opensandbox-group/OpenSandbox/pull/2915"
GUIDE_NOTE = "calibration provenance: docs/guides/density-tradeoff-benchmark.md"

BACKENDS = ("sim", "host")
SCENARIOS = ("density", "tighten-margins", "deflate-rescue")
POSTURES = ("redline", "elastic", "both")
WORKLOAD_KINDS = ("idle", "sparse", "bursty", "dense")
MIB_PER_GIB = 1024.0

# Dense profiles fill the guest at this rate (MiB/s), mirroring the ramping
# memory hog used in the tighten-ordering measurements.
DENSE_RAMP_MIB_PER_S = 100.0
# Dense ramp start times are staggered within this window.
DENSE_STAGGER_S = 8.0

# Forced red-line triggers for the tighten matrix, in elapsed seconds; the
# sampling cadence must resolve the earliest one (the inflate-first case).
MATRIX_TRIGGER_S = 25.0
MATRIX_INFLATE_FIRST_TRIGGER_S = 10.0
MATRIX_RELEASE_AFTER_S = 20.0


@dataclass(frozen=True)
class WorkloadProfile:
    """One guest workload shape.

    ``working_set_mib`` is transient anonymous memory the guest allocates and
    frees. ``period_s``/``hold_s`` schedule that allocation; ``busy_s`` and
    ``cpu_mcore`` schedule a CPU burst inside the same period. ``dense``
    ramps once to ``working_set_mib`` and holds it (full-inflation posture).
    """

    kind: str
    working_set_mib: float = 0.0
    period_s: float = 0.0
    hold_s: float = 0.0
    busy_s: float = 0.0
    cpu_mcore: float = 0.0

    def phase(self, t: float, offset: float) -> float:
        if self.period_s <= 0:
            return 0.0
        return (t + offset) % self.period_s

    def anon_target(self, t: float, offset: float) -> float:
        if self.kind == "idle":
            return 0.0
        if self.kind == "dense":
            return min(
                self.working_set_mib,
                DENSE_RAMP_MIB_PER_S * max(0.0, t - (offset % DENSE_STAGGER_S)),
            )
        phase = self.phase(t, offset)
        if phase < self.hold_s:
            return self.working_set_mib
        return 0.0

    def cpu_mcore_at(self, t: float, offset: float) -> float:
        if self.cpu_mcore <= 0:
            return 0.0
        if self.period_s <= 0:
            return self.cpu_mcore if self.busy_s > 0 else 0.0
        phase = self.phase(t, offset)
        return self.cpu_mcore if phase < self.busy_s else 0.0


# Defaults follow the workload shapes used in the OSEP-0033 evidence: idle
# fleets for density sweeps, a ~30%-busy mix for co-location, and ramping
# memory hogs for tighten-ordering measurements.
DEFAULT_PROFILES = {
    "idle": WorkloadProfile("idle"),
    "sparse": WorkloadProfile(
        "sparse",
        working_set_mib=128.0,
        period_s=60.0,
        hold_s=10.0,
        busy_s=10.0,
        cpu_mcore=200.0,
    ),
    "bursty": WorkloadProfile(
        "bursty",
        working_set_mib=256.0,
        period_s=120.0,
        hold_s=5.0,
        busy_s=12.0,
        cpu_mcore=1000.0,
    ),
    "dense": WorkloadProfile("dense"),
}


def parse_mix(spec: str) -> dict[str, float]:
    """Parse ``idle=0.8,sparse=0.1,bursty=0.1`` into a validated mix."""
    mix: dict[str, float] = {}
    for part in spec.split(","):
        part = part.strip()
        if not part:
            continue
        kind, sep, value = part.partition("=")
        if not sep or kind not in WORKLOAD_KINDS:
            raise ValueError(
                f"Unknown workload kind {kind!r}; expected one of {WORKLOAD_KINDS}"
            )
        mix[kind] = float(value)
    if not mix:
        raise ValueError("Empty workload mix")
    if any(v < 0 for v in mix.values()):
        raise ValueError("Workload mix fractions must be nonnegative")
    total = sum(mix.values())
    if not math.isclose(total, 1.0, rel_tol=0.0, abs_tol=1e-6):
        raise ValueError(f"Workload mix fractions must sum to 1.0 (got {total})")
    return mix


def format_mix(mix: dict[str, float]) -> str:
    return " ".join(f"{kind}={mix[kind]:.2f}" for kind in sorted(mix))


@dataclass(frozen=True)
class SimConfig:
    """One simulator configuration; see ``build_parser`` for defaults.

    Calibration fields carry provenance in the benchmark guide. Policy
    fields mirror the OSEP-0033 layers: red-line bands, the host-wide
    sigma-caps invariant, tighten margin, ordering, ``deflate_on_oom``, and
    the queued-create floor guarantee.
    """

    node_memory_mib: float
    platform_reserved_mib: float
    physical_cores: int
    cap_mib: float
    floor_mib: float
    interval_s: float
    duration_s: float
    seed: int
    create_rate: float
    mix: dict[str, float]
    snapshot_mib_per_sandbox: float
    storage_gib: float
    storage_headroom_pct: float
    postures: tuple[str, ...]
    scenario: str
    # calibration
    vmm_overhead_mib: float = 3.0
    idle_anon_mib: float = 2.2
    image_file_mib: float = 21.0
    file_reclaimable_fraction: float = 0.85
    fpr_delay_s: float = 6.0
    idle_cpu_mcore: float = 0.3
    # policy layers
    red_line_enter_pct: float = 15.0
    red_line_exit_pct: float = 18.0
    invariant_pct: float = 80.0
    tighten_margin_mib: float = 512.0
    oom_margin_threshold_mib: float = 512.0
    oom_occupancy_ratio: float = 0.5
    throttle_first: bool = True
    deflate_on_oom: bool = True
    floor_guarantee: bool = True
    restore_batch: int = 64
    retry_after_s: float = 2.0
    host_oom_reserve_mib: float | None = None
    # scenario helpers
    fill_ratio: float = 1.0
    force_redline_ticks: tuple[int, ...] = ()
    release_redline_tick: int | None = None


@dataclass
class SimSandbox:
    """One modeled micro-VM and its host-side memory accounting.

    ``anon`` is guest anonymous memory resident on the host (non-reclaimable
    without a balloon); ``file`` is guest file-backed memory (page-cache
    reclaimable). ``pending_reclaim`` holds anonymous pages the guest freed
    that free-page reporting has not yet returned to the host.
    """

    index: int
    cap_mib: float
    profile: WorkloadProfile
    offset: float
    anon: float
    file: float
    birth_tick: int = 0
    balloon: float = 0.0
    balloon_target: float | None = None
    balloon_pre: float | None = None
    throttled: bool = False
    tightened: bool = False
    workload_dead: bool = False
    oom_kills: int = 0
    deflate_saves: int = 0
    pending_reclaim: list = field(default_factory=list)
    busy_mcore: float = 0.0

    @property
    def effective_mib(self) -> float:
        return self.cap_mib - self.balloon

    @property
    def touched_mib(self) -> float:
        return self.anon + self.file

    def resident_mib(self, cfg: SimConfig) -> float:
        pending = sum(amount for _, amount in self.pending_reclaim)
        return cfg.vmm_overhead_mib + self.anon + pending + self.file

    def projected_charge_growth(self, cfg: SimConfig) -> float:
        """Upper bound on the charge this guest still adds once resumed.

        The profile's working set is the ceiling on anonymous memory, and the
        guest's file pages and VMM overhead are already resident, so the only
        growth left is anonymous memory up to the working-set target.
        """
        target = cfg.idle_anon_mib + self.profile.working_set_mib
        return max(0.0, target - self.anon)

    def charge_mib(self, cfg: SimConfig) -> float:
        """Memory the node cannot trivially reclaim (MemAvailable debit).

        Freed anonymous pages stay resident until free-page reporting returns
        them, so ``pending_reclaim`` is charged like live anonymous memory.
        """
        pending = sum(amount for _, amount in self.pending_reclaim)
        keep = 1.0 - cfg.file_reclaimable_fraction
        return cfg.vmm_overhead_mib + self.anon + pending + keep * self.file

    def tick(self, tick_idx: int, cfg: SimConfig) -> None:
        # A balloon move decided last tick actuates now (sub-second in the
        # measurements, modeled as one interval).
        if self.balloon_target is not None:
            self.apply_balloon_target(self.balloon_target, tick_idx, cfg)
            self.balloon_target = None

        fpr_ticks = max(1, round(cfg.fpr_delay_s / cfg.interval_s))
        while self.pending_reclaim and self.pending_reclaim[0][0] <= tick_idx:
            self.pending_reclaim.pop(0)

        if self.throttled:
            # SIGSTOP'd workload: memory and CPU stay where they are.
            self.busy_mcore = cfg.idle_cpu_mcore
            return

        # Workload clocks start at the guest's own admission: a late arrival
        # must ramp at the profile rate instead of inheriting the fleet's
        # elapsed time and materializing a full working set on arrival.
        t = (tick_idx - self.birth_tick) * cfg.interval_s
        if self.workload_dead:
            # A guest OOM killed the workload process; the micro-VM survives
            # but nothing restarts the allocation.
            target = cfg.idle_anon_mib
        else:
            target = cfg.idle_anon_mib + self.profile.anon_target(t, self.offset)

        if target > self.anon:
            need = target - self.anon
            room = self.effective_mib - self.touched_mib
            if need > room:
                deficit = need - room
                if cfg.deflate_on_oom and self.balloon > 0:
                    # deflate_on_oom is the guest-side tail insurance: the
                    # balloon releases at most what it holds, never more (that
                    # would push the effective cap above the declared cap).
                    released = min(self.balloon, deficit)
                    self.balloon -= released
                    deficit -= released
                    if deficit == 0:
                        self.deflate_saves += 1
                if deficit > 0:
                    # Guest OOM: the in-guest process dies, the micro-VM
                    # survives, freed pages return via FPR.
                    self.oom_kills += 1
                    freed = self.anon - cfg.idle_anon_mib
                    if freed > 0:
                        self.pending_reclaim.append((tick_idx + fpr_ticks, freed))
                    self.anon = cfg.idle_anon_mib
                    self.workload_dead = True
                    self.busy_mcore = cfg.idle_cpu_mcore
                    return
            self.anon += need
        elif target < self.anon:
            freed = self.anon - target
            self.anon = target
            if freed > 0:
                self.pending_reclaim.append((tick_idx + fpr_ticks, freed))

        if self.workload_dead:
            self.busy_mcore = cfg.idle_cpu_mcore
            return
        self.busy_mcore = cfg.idle_cpu_mcore + self.profile.cpu_mcore_at(t, self.offset)

    def apply_balloon_target(
        self, target: float, tick_idx: int, cfg: SimConfig
    ) -> None:
        """Actuate a balloon move decided by the red-line controller."""
        target = max(0.0, min(self.cap_mib, target))
        delta = target - self.balloon
        if delta <= 0:
            self.balloon = target
            self.tightened = False
            self.throttled = False
            return
        new_effective = self.cap_mib - target
        if self.touched_mib > new_effective and not self.throttled:
            # Ordering violated: the allocator was never stopped, so the
            # inflate lands into a guest that is still growing.
            self.guest_oom(tick_idx, cfg)
            self.balloon = target
            self.tightened = True
            return
        # The guest first drops page cache; any remainder comes from
        # untouched slack, which never reached the host.
        drop = min(delta, self.file)
        self.file -= drop
        self.balloon = target
        self.tightened = True

    def guest_oom(self, tick_idx: int, cfg: SimConfig) -> None:
        self.oom_kills += 1
        freed = self.anon - cfg.idle_anon_mib
        if freed > 0:
            fpr_ticks = max(1, round(cfg.fpr_delay_s / cfg.interval_s))
            self.pending_reclaim.append((tick_idx + fpr_ticks, freed))
        self.anon = cfg.idle_anon_mib
        self.workload_dead = True
        self.throttled = False


@dataclass
class TierCounters:
    granted: int = 0
    invariant_denied: int = 0
    redline_denied: int = 0
    floor_grants: int = 0
    queue_pending_end: int = 0
    redline_events: int = 0
    ticks_redline: int = 0
    tighten_actions: int = 0
    tighten_skips: int = 0
    restored: int = 0
    guest_oom_kills: int = 0
    host_oom_kills: int = 0
    deflate_saves: int = 0


@dataclass
class TierResult:
    posture: str
    requested_sandboxes: int
    placed_sandboxes: int
    counters: TierCounters
    samples: list
    resident_mean_mib: float
    resident_mean_per_sandbox_mib: float | None
    resident_max_mib: float
    mem_available_min_pct: float
    mem_available_mean_pct: float
    psi_cpu_some_max_pct: float
    psi_cpu_full_max_pct: float
    psi_mem_some_max_pct: float
    peak_busy_cores: float
    redline_unrecovered: bool = False

    def to_summary(self, cfg: SimConfig, scenario: str) -> dict:
        oversell_requested = (
            self.requested_sandboxes * cfg.cap_mib / cfg.node_memory_mib
        )
        oversell_placed = self.placed_sandboxes * cfg.cap_mib / cfg.node_memory_mib
        storage_gib = self.placed_sandboxes * cfg.snapshot_mib_per_sandbox / MIB_PER_GIB
        storage_limit_gib = cfg.storage_gib * (1.0 - cfg.storage_headroom_pct / 100.0)
        violations = []
        if self.counters.host_oom_kills:
            violations.append("host_oom_kills")
        if self.counters.guest_oom_kills:
            violations.append("guest_oom_kills")
        if self.mem_available_min_pct <= 0.0:
            violations.append("memory_exhausted")
        if self.redline_unrecovered:
            violations.append("red_line_unrecovered")
        if storage_gib > storage_limit_gib:
            violations.append("storage_over_limit")
        return {
            "type": "summary",
            "scenario": scenario,
            "backend": "sim",
            "posture": self.posture,
            "requested_sandboxes": self.requested_sandboxes,
            "placed_sandboxes": self.placed_sandboxes,
            "requested_oversell_ratio": round(oversell_requested, 4),
            "placed_oversell_ratio": round(oversell_placed, 4),
            "cap_mib": cfg.cap_mib,
            "floor_mib": cfg.floor_mib,
            "mix": dict(cfg.mix),
            "horizon": {
                "duration_s": cfg.duration_s,
                "interval_s": cfg.interval_s,
                "seed": cfg.seed,
                "create_rate_per_s": cfg.create_rate,
            },
            # The full effective configuration, so a saved summary remains
            # reproducible without re-stating every override alongside it.
            "config": {
                "node_memory_mib": cfg.node_memory_mib,
                "platform_reserved_mib": cfg.platform_reserved_mib,
                "physical_cores": cfg.physical_cores,
                "red_line_enter_pct": cfg.red_line_enter_pct,
                "red_line_exit_pct": cfg.red_line_exit_pct,
                "invariant_pct": cfg.invariant_pct,
                "tighten_margin_mib": cfg.tighten_margin_mib,
                "oom_margin_threshold_mib": cfg.oom_margin_threshold_mib,
                "oom_occupancy_ratio": cfg.oom_occupancy_ratio,
                "throttle_first": cfg.throttle_first,
                "deflate_on_oom": cfg.deflate_on_oom,
                "floor_guarantee": cfg.floor_guarantee,
                "fill_ratio": cfg.fill_ratio,
                "snapshot_mib_per_sandbox": cfg.snapshot_mib_per_sandbox,
                "storage_gib": cfg.storage_gib,
                "storage_headroom_pct": cfg.storage_headroom_pct,
            },
            "host_oom_reserve_mib": round(host_oom_reserve(cfg), 3),
            "resident_mib": {
                "mean_total": round(self.resident_mean_mib, 3),
                "max_total": round(self.resident_max_mib, 3),
                # Averaged over live guests per tick, so ramped fleets are
                # not diluted by ticks before the fleet finished building.
                # Gated on whether any live-guest samples were collected, not
                # on the final survivor count: a tier whose last guests are
                # host-evicted mid-run still recorded a real per-sandbox cost.
                "mean_per_sandbox": None
                if self.resident_mean_per_sandbox_mib is None
                else round(self.resident_mean_per_sandbox_mib, 3),
            },
            "peak_resident_pct": round(
                100.0 * self.resident_max_mib / cfg.node_memory_mib, 4
            ),
            "mem_available_pct": {
                "min": round(self.mem_available_min_pct, 4),
                "mean": round(self.mem_available_mean_pct, 4),
                "final": self.samples[-1]["mem_available_pct"]
                if self.samples
                else None,
            },
            "psi_max_pct": {
                "cpu_some": round(self.psi_cpu_some_max_pct, 4),
                "cpu_full": round(self.psi_cpu_full_max_pct, 4),
                "mem_some": round(self.psi_mem_some_max_pct, 4),
            },
            "peak_busy_cores": round(self.peak_busy_cores, 3),
            "admission": {
                "granted": self.counters.granted,
                "invariant_denied": self.counters.invariant_denied,
                "redline_denied": self.counters.redline_denied,
                "floor_grants": self.counters.floor_grants,
                "queue_pending_end": self.counters.queue_pending_end,
            },
            "redline": {
                "events": self.counters.redline_events,
                "ticks_active": self.counters.ticks_redline,
                "tighten_actions": self.counters.tighten_actions,
                "tighten_skips": self.counters.tighten_skips,
                "restored": self.counters.restored,
                "unrecovered": self.redline_unrecovered,
            },
            "events": {
                "guest_oom_kills": self.counters.guest_oom_kills,
                "host_oom_kills": self.counters.host_oom_kills,
                "deflate_saves": self.counters.deflate_saves,
            },
            "storage_gib": {
                "total": round(storage_gib, 3),
                "limit": round(storage_limit_gib, 3),
            },
            "verdict": {"ok": not violations, "violations": violations},
        }


def assign_kinds(n: int, mix: dict[str, float], rng: random.Random) -> list[str]:
    """Deterministically split n sandboxes across the workload mix."""
    counts = {kind: int(fraction * n) for kind, fraction in mix.items()}
    assigned = sum(counts.values())
    kinds_ordered = sorted(mix, key=lambda k: (-mix[k], k))
    idx = 0
    while assigned < n:
        counts[kinds_ordered[idx % len(kinds_ordered)]] += 1
        assigned += 1
        idx += 1
    kinds: list[str] = []
    for kind in sorted(counts):
        kinds.extend([kind] * counts[kind])
    rng.shuffle(kinds)
    return kinds


def invariant_limit_mib(cfg: SimConfig) -> float:
    """Sigma-caps budget. The host-wide invariant charges the platform
    baseline against the same budget (OSEP-0033, node invariant), so
    simultaneous full inflation still fits the node by construction."""
    return max(
        0.0,
        cfg.invariant_pct / 100.0 * cfg.node_memory_mib - cfg.platform_reserved_mib,
    )


def resolve_profile(kind: str, cfg: SimConfig) -> WorkloadProfile:
    profile = DEFAULT_PROFILES[kind]
    if kind == "dense":
        # Fill to fill_ratio of the declared cap; the guest already holds
        # idle anonymous pages and image file pages, so subtract both.
        working_set = max(
            0.0,
            cfg.cap_mib * cfg.fill_ratio - cfg.idle_anon_mib - cfg.image_file_mib,
        )
        profile = WorkloadProfile("dense", working_set_mib=working_set)
    return profile


def plan_tighten(
    sandbox: SimSandbox,
    tick_idx: int,
    cfg: SimConfig,
    margin: float,
    counters: TierCounters,
) -> float | None:
    """Balloon amount for the tighten step, or None to skip (no action).

    OSEP-0033 derivation: ``amount = cap - (touched + margin)`` — the floor
    value is never programmed into the balloon directly.
    """
    floor = sandbox.touched_mib + margin
    amount = sandbox.cap_mib - floor
    if amount <= 0:
        counters.tighten_skips += 1
        return None
    if amount <= sandbox.balloon:
        # An elastic sandbox may already hold a larger balloon than the
        # tighten target. Deflating it would hand memory back while the
        # pressure persists, so leave the balloon and the throttle alone.
        counters.tighten_skips += 1
        return None
    # Growth still pending in the next interval is what a late throttle
    # would let run into the inflate window. The guest clock runs from its
    # own admission, like tick() does, and a dead workload is not allocating:
    # nothing restarts it, so counting it again would recount the same kill.
    allocating = not sandbox.workload_dead and (
        sandbox.profile.anon_target(
            (tick_idx + 1 - sandbox.birth_tick) * cfg.interval_s, sandbox.offset
        )
        > sandbox.anon
    )
    if not cfg.throttle_first and allocating:
        # Throttle must land before inflation; without it the allocator
        # keeps growing into the inflate window (measured failure).
        sandbox.guest_oom(tick_idx, cfg)
        return None
    if (
        cfg.throttle_first
        and margin < cfg.oom_margin_threshold_mib
        and sandbox.touched_mib > cfg.oom_occupancy_ratio * sandbox.cap_mib
    ):
        # Measured failure law: a margin below the measured-safe threshold
        # on a deeply occupied guest OOMs even with ordering enforced.
        # Encoded, not re-derived.
        sandbox.guest_oom(tick_idx, cfg)
        return None
    counters.tighten_actions += 1
    return amount


def boot_footprint_mib(cfg: SimConfig) -> float:
    """Guest-side memory a sandbox needs before its workload starts.

    The elastic floor (and any inflate-derived effective cap) must cover it:
    a guest born with an effective cap below its own footprint would sit
    permanently over-cap without ever being reclaimed or reported.
    """
    return cfg.idle_anon_mib + cfg.image_file_mib


def host_oom_reserve(cfg: SimConfig) -> float:
    """Last-resort eviction threshold, scaled to the configured node.

    The default is 0.8% of node memory with a 256 MiB floor, capped at 90%
    of the configured red-line enter band so eviction can never preempt the
    state machine; on a small node the cap can pull the threshold below the
    floor. An explicit override is used as-is.
    """
    if cfg.host_oom_reserve_mib is not None:
        return cfg.host_oom_reserve_mib
    auto = max(256.0, 0.008 * cfg.node_memory_mib)
    ceiling = 0.9 * (cfg.red_line_enter_pct / 100.0) * cfg.node_memory_mib
    return min(auto, ceiling)


def run_tier(cfg: SimConfig, posture: str, requested_n: int) -> TierResult:
    """Simulate one density tier under one posture."""
    if cfg.floor_mib < boot_footprint_mib(cfg) - 1e-9:
        raise ValueError(
            "floor_mib must cover the modeled boot footprint "
            f"({boot_footprint_mib(cfg):g} MiB)"
        )
    rng = random.Random(f"{cfg.seed}:{posture}:{requested_n}")
    counters = TierCounters()
    kinds = assign_kinds(requested_n, cfg.mix, rng)
    cap = cfg.cap_mib
    elastic = posture == "elastic"
    floor_balloon = max(0.0, cap - cfg.floor_mib)
    invariant_budget = invariant_limit_mib(cfg)
    ticks = max(1, round(cfg.duration_s / cfg.interval_s))
    retry_ticks = max(1, round(cfg.retry_after_s / cfg.interval_s))

    sandboxes: list[SimSandbox] = []
    caps_committed = 0.0
    redline = False
    draining = False
    evicted_oom_kills = 0
    evicted_deflate_saves = 0

    def attempt(kind: str, tick_idx: int, retry_queue: deque) -> None:
        nonlocal caps_committed
        if elastic and caps_committed + cap > invariant_budget + 1e-9:
            counters.invariant_denied += 1
            return
        if redline and not cfg.floor_guarantee:
            counters.redline_denied += 1
            retry_queue.append((tick_idx + retry_ticks, kind))
            return
        profile = resolve_profile(kind, cfg)
        offset = rng.uniform(0.0, max(profile.period_s, 8.0))
        floor_grant = redline  # floor guarantee grants during the freeze
        balloon0 = floor_balloon if (elastic or floor_grant) else 0.0
        sandboxes.append(
            SimSandbox(
                index=len(sandboxes),
                cap_mib=cap,
                profile=profile,
                offset=offset,
                anon=cfg.idle_anon_mib,
                file=cfg.image_file_mib,
                birth_tick=tick_idx,
                balloon=balloon0,
            )
        )
        caps_committed += cap
        counters.granted += 1
        if floor_grant:
            counters.floor_grants += 1
            grant = sandboxes[-1]
            if not elastic:
                # A redline-posture floor grant starts ballooned only until
                # the red line clears: mark it so the drain deflates it back
                # to the posture's full-cap baseline.
                grant.tightened = True
                grant.balloon_pre = 0.0
            else:
                # Elastic guests keep their floor balloon by design; the
                # drain must return them to that level, not to full cap.
                grant.balloon_pre = grant.balloon
            if cfg.throttle_first:
                # A guest admitted mid-freeze joins the freeze. Otherwise the
                # tighten pass, which only runs on entry, never sees it, and
                # deflate_on_oom would release its floor balloon on the first
                # demand spike, growing the guest toward its cap while the
                # node is still under red-line pressure.
                grant.throttled = True

    def tighten_plan(sandbox: SimSandbox, tick_idx: int, margin: float) -> float | None:
        return plan_tighten(sandbox, tick_idx, cfg, margin, counters)

    arrival_queue: deque = deque(kinds)
    retry_queue: deque = deque()
    next_arrival = 0.0

    samples: list = []
    resident_sum = 0.0
    resident_per_sandbox_sum = 0.0
    resident_per_sandbox_ticks = 0
    resident_max = 0.0
    avail_min = 100.0
    avail_sum = 0.0
    psi_some_max = 0.0
    psi_full_max = 0.0
    psi_mem_max = 0.0
    busy_max = 0.0

    for tick_idx in range(ticks):
        # --- admission ------------------------------------------------------
        while retry_queue and retry_queue[0][0] <= tick_idx:
            _, kind = retry_queue.popleft()
            attempt(kind, tick_idx, retry_queue)
        if cfg.create_rate <= 0:
            while arrival_queue:
                attempt(arrival_queue.popleft(), tick_idx, retry_queue)
        else:
            next_arrival += cfg.create_rate * cfg.interval_s
            while next_arrival >= 1.0 and arrival_queue:
                next_arrival -= 1.0
                attempt(arrival_queue.popleft(), tick_idx, retry_queue)
        counters.queue_pending_end = len(retry_queue) + len(arrival_queue)

        # --- fleet ----------------------------------------------------------
        fleet_charge = 0.0
        busy_mcore = 0.0
        for sandbox in sandboxes:
            sandbox.tick(tick_idx, cfg)
            fleet_charge += sandbox.charge_mib(cfg)
            busy_mcore += sandbox.busy_mcore

        available = cfg.node_memory_mib - cfg.platform_reserved_mib - fleet_charge
        avail_pct = 100.0 * available / cfg.node_memory_mib

        # --- red-line state machine ------------------------------------------
        # The state machine reacts to a fresh crossing before last-resort
        # eviction runs: a single tick can step over both the enter band and
        # the eviction reserve, and eviction must never pre-empt the freeze
        # that the policy prescribes for exactly that crossing.
        forced_on = tick_idx in cfg.force_redline_ticks
        forced_off = cfg.release_redline_tick == tick_idx
        # A synthetic trigger holds the red line until its release tick even
        # when headroom is high (the matrix cases freeze on a relaxed node);
        # runs without a release tick exit through the normal bands.
        forced_window = (
            cfg.release_redline_tick is not None and tick_idx < cfg.release_redline_tick
        )
        if redline and (
            forced_off or (avail_pct >= cfg.red_line_exit_pct and not forced_window)
        ):
            redline = False
            draining = True
        if not redline and (forced_on or avail_pct < cfg.red_line_enter_pct):
            # Re-entry is allowed mid-drain: resumed guests can push the node
            # back under the enter band before the last batch is restored, so
            # the drain is aborted and the fleet is re-frozen.
            draining = False
            redline = True
            counters.redline_events += 1
            for sandbox in sandboxes:
                # Capture the pre-entry balloon even for no-op plans, so the
                # restore path returns elastic guests to their floor posture
                # instead of deflating them to zero. A guest still tightened
                # or throttled from a drain that re-entry aborted keeps its
                # original pre-cycle value.
                already_engaged = sandbox.tightened or sandbox.throttled
                if cfg.throttle_first:
                    sandbox.throttled = True
                if not already_engaged:
                    sandbox.balloon_pre = sandbox.balloon
                plan = tighten_plan(sandbox, tick_idx, cfg.tighten_margin_mib)
                if plan is not None:
                    sandbox.balloon_target = plan
        # --- host OOM (last-resort eviction) ---------------------------------
        oom_reserve = host_oom_reserve(cfg)
        if available < oom_reserve and sandboxes:
            while available < oom_reserve and sandboxes:
                victim = max(sandboxes, key=lambda s: s.charge_mib(cfg))
                sandboxes.remove(victim)
                caps_committed -= victim.cap_mib
                evicted_oom_kills += victim.oom_kills
                evicted_deflate_saves += victim.deflate_saves
                counters.host_oom_kills += 1
                fleet_charge = sum(s.charge_mib(cfg) for s in sandboxes)
                available = (
                    cfg.node_memory_mib - cfg.platform_reserved_mib - fleet_charge
                )
            avail_pct = 100.0 * available / cfg.node_memory_mib

        if redline:
            counters.ticks_redline += 1
        if draining:
            pending = [s for s in sandboxes if s.tightened or s.throttled]
            # Resume only while the node can absorb the batch's full growth:
            # last-resort eviction runs ahead of the next re-entry check, so a
            # batch that would dive through the enter band would be evicted
            # before the red line could re-freeze it. Guests that do not fit
            # stay frozen and are retried on later ticks.
            projected_available = available
            for sandbox in pending[: max(1, cfg.restore_batch)]:
                growth = sandbox.projected_charge_growth(cfg)
                projected_pct = (
                    100.0 * (projected_available - growth) / cfg.node_memory_mib
                )
                if projected_pct < cfg.red_line_enter_pct:
                    continue
                sandbox.balloon_target = (
                    sandbox.balloon_pre if sandbox.balloon_pre is not None else 0.0
                )
                counters.restored += 1
                projected_available -= growth
            if not pending:
                draining = False

        # --- PSI projection ---------------------------------------------------
        util = (busy_mcore / 1000.0) / max(1, cfg.physical_cores)
        psi_some = min(100.0, 55.0 * util * util)
        psi_full = min(100.0, max(0.0, (util - 1.0) * 100.0))
        psi_mem = min(100.0, max(0.0, (0.25 - avail_pct / 100.0) * 400.0))

        resident_total = sum(s.resident_mib(cfg) for s in sandboxes)
        balloon_total = sum(s.balloon for s in sandboxes)
        resident_sum += resident_total
        if sandboxes:
            resident_per_sandbox_sum += resident_total / len(sandboxes)
            resident_per_sandbox_ticks += 1
        resident_max = max(resident_max, resident_total)
        avail_min = min(avail_min, avail_pct)
        avail_sum += avail_pct
        psi_some_max = max(psi_some_max, psi_some)
        psi_full_max = max(psi_full_max, psi_full)
        psi_mem_max = max(psi_mem_max, psi_mem)
        busy_max = max(busy_max, busy_mcore / 1000.0)
        counters.guest_oom_kills = evicted_oom_kills + sum(
            s.oom_kills for s in sandboxes
        )
        counters.deflate_saves = evicted_deflate_saves + sum(
            s.deflate_saves for s in sandboxes
        )

        samples.append(
            {
                "type": "sample",
                "tick": tick_idx,
                "t_s": round(tick_idx * cfg.interval_s, 3),
                "sandboxes": len(sandboxes),
                "resident_mib": round(resident_total, 3),
                "balloon_mib": round(balloon_total, 3),
                "mem_available_pct": round(avail_pct, 4),
                "psi_cpu_some_pct": round(psi_some, 4),
                "psi_cpu_full_pct": round(psi_full, 4),
                "psi_mem_some_pct": round(psi_mem, 4),
                "redline_active": redline,
                "guest_oom_kills": counters.guest_oom_kills,
                "host_oom_kills": counters.host_oom_kills,
            }
        )

    return TierResult(
        posture=posture,
        requested_sandboxes=requested_n,
        placed_sandboxes=len(sandboxes),
        counters=counters,
        samples=samples,
        resident_mean_mib=resident_sum / ticks,
        resident_mean_per_sandbox_mib=(
            resident_per_sandbox_sum / resident_per_sandbox_ticks
            if resident_per_sandbox_ticks
            else None
        ),
        resident_max_mib=resident_max,
        mem_available_min_pct=avail_min,
        mem_available_mean_pct=avail_sum / ticks,
        psi_cpu_some_max_pct=psi_some_max,
        psi_cpu_full_max_pct=psi_full_max,
        psi_mem_some_max_pct=psi_mem_max,
        peak_busy_cores=busy_max,
        redline_unrecovered=redline or draining,
    )


# ---------------------------------------------------------------------------
# Scenarios
# ---------------------------------------------------------------------------


def run_density_scenario(cfg: SimConfig, ratios: list[float]) -> list[TierResult]:
    results = []
    for posture in cfg.postures:
        for ratio in ratios:
            n = max(1, round(ratio * cfg.node_memory_mib / cfg.cap_mib))
            print(
                f"sim: posture={posture} oversell={ratio:.2f}x sandboxes={n}",
                file=sys.stderr,
            )
            results.append(run_tier(cfg, posture, n))
    return results


@dataclass(frozen=True)
class MatrixRow:
    """One tighten-ordering matrix case: its config and expected outcome."""

    case: str
    cap_mib: float
    margin_mib: float
    throttle_first: bool
    force_tick: int
    expect_oom: bool
    expect_skip: bool
    config: SimConfig
    result: TierResult

    def to_row(self) -> dict:
        # Serialize against this row's own configuration so cap, mix, and
        # oversell match the run that produced the counters.
        summary = self.result.to_summary(self.config, "tighten-margins")
        inflated = self.result.counters.tighten_actions
        ok = (
            (self.result.counters.guest_oom_kills > 0) == self.expect_oom
            and (self.result.counters.tighten_skips > 0) == self.expect_skip
            and self.result.counters.host_oom_kills == 0
        )
        return {
            "type": "matrix-row",
            "case": self.case,
            "cap_mib": self.cap_mib,
            "tighten_margin_mib": self.margin_mib,
            "ordering": "throttle-first" if self.throttle_first else "inflate-first",
            "sandboxes": self.result.placed_sandboxes,
            "force_redline_tick": self.force_tick,
            "tighten_actions": inflated,
            "tighten_skips": self.result.counters.tighten_skips,
            "guest_oom_kills": self.result.counters.guest_oom_kills,
            "host_oom_kills": self.result.counters.host_oom_kills,
            "expected_oom": self.expect_oom,
            "expected_skip": self.expect_skip,
            "verdict": {"ok": ok},
            "summary": summary,
        }


def run_tighten_matrix(cfg: SimConfig) -> list[MatrixRow]:
    """Tighten-ordering cases from the OSEP-0033 Test Plan (E8):

    - a margin at the measured-safe threshold holds with zero guest OOM;
    - the known-bad 128 MiB margin keeps the failure visible;
    - an infeasible floor (floor above the cap) emits skip, never an inflate;
    - inflate-before-throttle ordering OOMs guests that are mid-allocation.

    The synthetic red-line trigger is pinned to elapsed seconds (not tick
    indexes), so ``--interval`` changes the sampling cadence without changing
    which case runs; a horizon shorter than the case needs is extended so the
    forced window and its release always fit. A cadence coarser than the
    earliest trigger cannot resolve it and is rejected.
    """
    if cfg.interval_s > MATRIX_INFLATE_FIRST_TRIGGER_S:
        raise ValueError(
            "the tighten matrix's forced triggers need --interval <= "
            f"{MATRIX_INFLATE_FIRST_TRIGGER_S:g} s so the earliest trigger "
            "lands on its elapsed time"
        )
    cases = [
        # case, cap, margin, ordered, force at (s), expected OOM, expected skip
        ("safe margin, ordered", 2048.0, 512.0, True, MATRIX_TRIGGER_S, False, False),
        (
            "known-bad margin, ordered",
            2048.0,
            128.0,
            True,
            MATRIX_TRIGGER_S,
            True,
            False,
        ),
        (
            "infeasible floor, ordered",
            512.0,
            512.0,
            True,
            MATRIX_TRIGGER_S,
            False,
            True,
        ),
        (
            "thin margin on a 512 MiB cap",
            512.0,
            128.0,
            True,
            MATRIX_TRIGGER_S,
            True,
            False,
        ),
        (
            "safe margin, inflate-first",
            2048.0,
            512.0,
            False,
            MATRIX_INFLATE_FIRST_TRIGGER_S,
            True,
            False,
        ),
    ]
    rows = []
    for case, cap, margin, ordered, force_s, expect_oom, expect_skip in cases:
        force_tick = max(1, round(force_s / cfg.interval_s))
        release_tick = force_tick + max(
            1, round(MATRIX_RELEASE_AFTER_S / cfg.interval_s)
        )
        # A tail after the release lets the drained guests be observed.
        needed_s = (release_tick + 5) * cfg.interval_s
        row_cfg = replace(
            cfg,
            scenario="tighten-margins",
            postures=("redline",),
            cap_mib=cap,
            floor_mib=min(cfg.floor_mib, cap),
            snapshot_mib_per_sandbox=cap,  # dense snapshot is 1:1 with the cap
            mix={"dense": 1.0},
            create_rate=0.0,
            tighten_margin_mib=margin,
            throttle_first=ordered,
            fill_ratio=0.7,
            host_oom_reserve_mib=64.0,
            duration_s=max(cfg.duration_s, needed_s),
            force_redline_ticks=(force_tick,),
            release_redline_tick=release_tick,
        )
        # The matrix runs on a tiny fleet so the case is the only variable.
        result = run_tier(row_cfg, "redline", 8)
        rows.append(
            MatrixRow(
                case=case,
                cap_mib=cap,
                margin_mib=margin,
                throttle_first=ordered,
                force_tick=force_tick,
                expect_oom=expect_oom,
                expect_skip=expect_skip,
                config=row_cfg,
                result=result,
            )
        )
    return rows


def run_deflate_rescue(cfg: SimConfig) -> tuple[SimConfig, list[dict]]:
    """deflate_on_oom tail insurance (E5): a burst beyond the effective
    (balloon-reduced) cap is absorbed by automatic deflation; with the
    escape hatch off the same burst OOMs the guest process.

    Returns the shared row config so the report header describes the
    experiment that actually ran, not the top-level defaults, plus the
    per-row results for JSONL sample output.
    """
    rows = []
    results = []
    base = replace(
        cfg,
        scenario="deflate-rescue",
        postures=("elastic",),
        mix={"dense": 1.0},
        cap_mib=2048.0,
        floor_mib=1024.0,
        snapshot_mib_per_sandbox=2048.0,
        fill_ratio=0.6,
        create_rate=0.0,
    )
    # The comparison means nothing until the dense guest has ramped to the
    # balloon-reduced effective cap (E5), so a horizon too short to reach the
    # burst is extended in whole ticks, the way the tighten matrix extends for
    # its window; a coarse cadence must still sample at or after the burst.
    profile = resolve_profile("dense", base)
    burst_s = profile.working_set_mib / DENSE_RAMP_MIB_PER_S + DENSE_STAGGER_S + 5.0
    burst_ticks = math.ceil(burst_s / cfg.interval_s) + 2
    base = replace(base, duration_s=max(cfg.duration_s, burst_ticks * cfg.interval_s))
    for deflate_on in (True, False):
        row_cfg = replace(base, deflate_on_oom=deflate_on)
        result = run_tier(row_cfg, "elastic", 1)
        results.append(result)
        # The advertised comparison needs the guest to survive the burst: a
        # host-evicted sandbox absorbed nothing.
        rescued = (
            result.counters.deflate_saves > 0
            and result.counters.guest_oom_kills == 0
            and result.counters.host_oom_kills == 0
            and result.placed_sandboxes > 0
        )
        oomed = (
            result.counters.guest_oom_kills > 0 or result.counters.host_oom_kills > 0
        )
        rows.append(
            {
                "type": "matrix-row",
                "case": "burst beyond effective cap",
                "deflate_on_oom": deflate_on,
                "sandboxes": result.placed_sandboxes,
                "deflate_saves": result.counters.deflate_saves,
                "guest_oom_kills": result.counters.guest_oom_kills,
                "host_oom_kills": result.counters.host_oom_kills,
                "verdict": {"ok": rescued if deflate_on else oomed},
                "summary": result.to_summary(row_cfg, "deflate-rescue"),
            }
        )
    return base, rows, results


# ---------------------------------------------------------------------------
# Host backend (read-only sampler)
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class HostConfig:
    proc_root: Path
    cgroup_root: Path | None
    duration_s: float
    interval_s: float
    cap_mib: float
    sandboxes_override: int | None


def read_meminfo(proc_root: Path) -> dict:
    """Parse /proc/meminfo; values are reported in MiB."""
    values: dict[str, float] = {}
    for line in (proc_root / "meminfo").read_text(encoding="utf-8").splitlines():
        key, _, rest = line.partition(":")
        parts = rest.split()
        if parts:
            values[key.strip()] = float(parts[0])
    return {
        "mem_total_mib": values["MemTotal"] / 1024.0,
        "mem_available_mib": values["MemAvailable"] / 1024.0,
    }


def read_pressure(path: Path) -> dict:
    """Parse a PSI file (/proc/pressure/* or cgroup memory.pressure)."""
    out: dict[str, float] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        fields = line.split()
        if not fields:
            continue
        name = fields[0]
        for item in fields[1:]:
            key, _, value = item.partition("=")
            out[f"{name}_{key}"] = float(value)
    return out


def read_vmstat_oom_kills(proc_root: Path) -> int | None:
    vmstat = proc_root / "vmstat"
    if not vmstat.exists():
        return None
    for line in vmstat.read_text(encoding="utf-8").splitlines():
        fields = line.split()
        if len(fields) == 2 and fields[0] == "oom_kill":
            return int(fields[1])
    return None


def list_cgroup_dirs(root: Path) -> list[Path]:
    return sorted(
        path
        for path in root.iterdir()
        if path.is_dir() and (path / "memory.current").exists()
    )


def read_cgroups(root: Path) -> dict[str, dict]:
    """Read every sandbox cgroup under ``root``, tolerating churn.

    A sandbox that vanishes (or is recreated) between the directory listing
    and the file reads is skipped instead of aborting the sample.
    """
    samples: dict[str, dict] = {}
    for path in list_cgroup_dirs(root):
        try:
            samples[str(path)] = read_cgroup_sample(path)
        except FileNotFoundError:
            continue
    return samples


def read_cgroup_sample(path: Path) -> dict:
    # The directory inode is the cgroup's generation identity: a sandbox
    # recreated at the same path starts a fresh counter, and only a changed
    # inode distinguishes that replacement from a monotonic counter.
    inode = path.stat().st_ino
    current_kib = float((path / "memory.current").read_text(encoding="utf-8").strip())
    maximum = None
    max_file = path / "memory.max"
    if max_file.exists():
        raw = max_file.read_text(encoding="utf-8").strip()
        maximum = None if raw == "max" else float(raw)
    oom_kill = 0
    events = path / "memory.events"
    if events.exists():
        for line in events.read_text(encoding="utf-8").splitlines():
            fields = line.split()
            if len(fields) == 2 and fields[0] == "oom_kill":
                oom_kill = int(fields[1])
    sample = {
        "current_mib": current_kib / (1024.0 * 1024.0),
        "max_mib": None if maximum is None else maximum / (1024.0 * 1024.0),
        "oom_kill": oom_kill,
        "inode": inode,
    }
    pressure = path / "memory.pressure"
    if pressure.exists():
        parsed = read_pressure(pressure)
        sample["mem_some_avg60"] = parsed.get("some_avg60")
    return sample


def accumulate_cgroup_oom(
    state: dict[str, tuple[int | None, int]],
    seen: set[str],
    current: dict[str, tuple[int | None, int]],
    *,
    baseline_new_paths: bool,
) -> int:
    """Return cgroup OOM kills newly observed since the previous sample.

    ``state`` carries the last ``(directory inode, cumulative counter)`` pair
    per still-present cgroup, so kills are counted even when a cgroup is
    deleted later in the run. The inode is the generation identity: a cgroup
    recreated at the same path between two polls starts a fresh counter, and
    without the identity a replacement whose counter reached the old value
    (old ``1``, new ``1`` after one new kill) would be silently dropped. A
    filesystem that hands the recycled inode number to the replacement
    degrades to the counter-only semantics below.

    Paths present at the first sample are baselined (their pre-existing
    counters are not attributed to this run); paths that appear later count
    from zero, as do returning, counter-resetting, or recreated paths.
    """
    observed = 0
    for path, (inode, count) in current.items():
        previous = state.get(path)
        if previous is None:
            if path in seen or not baseline_new_paths:
                observed += count
            else:
                seen.add(path)
        else:
            previous_inode, previous_count = previous
            recreated = (
                inode is not None
                and previous_inode is not None
                and inode != previous_inode
            )
            if recreated or count < previous_count:
                # A replacement (or an in-place counter reset) starts over;
                # its current value is entirely new.
                observed += count
            else:
                observed += count - previous_count
        state[path] = (inode, count)
    # Forget vanished cgroups; ``seen`` keeps their history for recreation.
    for path in list(state):
        if path not in current:
            del state[path]
    return observed


def collect_host_samples(cfg: HostConfig) -> list[dict]:
    """Sample a live node until ``duration_s`` elapses."""
    samples = []
    oom_state: dict[str, tuple[int | None, int]] = {}
    oom_paths_seen: set[str] = set()
    cgroup_oom_seen = 0
    deadline = time.monotonic() + cfg.duration_s
    while True:
        meminfo = read_meminfo(cfg.proc_root)
        if meminfo["mem_total_mib"] <= 0:
            raise ValueError("Node reports nonpositive MemTotal; refusing to sample")
        cpu = read_pressure(cfg.proc_root / "pressure" / "cpu")
        memory = read_pressure(cfg.proc_root / "pressure" / "memory")
        # The cgroup counters are read before vmstat: the first vmstat read is
        # the system baseline, and a kill landing between the two reads must
        # not be baselined out of the fleet counters only to surface later as
        # a node-level delta (the fleet read starts at or after the baseline).
        if cfg.cgroup_root:
            cgroup_samples = read_cgroups(cfg.cgroup_root)
            counts = {
                path: sample["oom_kill"] for path, sample in cgroup_samples.items()
            }
            entries = {
                path: (sample["inode"], sample["oom_kill"])
                for path, sample in cgroup_samples.items()
            }
            cgroup_oom_seen += accumulate_cgroup_oom(
                oom_state,
                oom_paths_seen,
                entries,
                baseline_new_paths=not samples,  # first sample only
            )
            num_cgroups = len(cgroup_samples)
            cgroup_current = round(
                sum(item["current_mib"] for item in cgroup_samples.values()), 3
            )
            cgroup_oom_now = sum(counts.values())
        else:
            num_cgroups = None
            cgroup_current = None
            cgroup_oom_now = None
        vmstat_oom = read_vmstat_oom_kills(cfg.proc_root)
        samples.append(
            {
                "type": "sample",
                "t_s": round(time.monotonic() - (deadline - cfg.duration_s), 3),
                "mem_available_mib": round(meminfo["mem_available_mib"], 3),
                "mem_total_mib": round(meminfo["mem_total_mib"], 3),
                "psi_cpu_some_avg60": cpu.get("some_avg60"),
                "psi_cpu_full_avg60": cpu.get("full_avg60"),
                "psi_mem_some_avg60": memory.get("some_avg60"),
                "vmstat_oom_kill": vmstat_oom,
                "cgroups": num_cgroups,
                "cgroup_current_mib": cgroup_current,
                "cgroup_oom_kill": cgroup_oom_now,
                "cgroup_oom_kill_accumulated": cgroup_oom_seen
                if cfg.cgroup_root
                else None,
            }
        )
        if time.monotonic() >= deadline:
            break
        time.sleep(min(cfg.interval_s, max(0.0, deadline - time.monotonic())))
    return samples


def summarize_host_samples(samples: list[dict], cfg: HostConfig) -> dict:
    mem_total = samples[0]["mem_total_mib"]
    mem_avail = [s["mem_available_mib"] for s in samples]
    cgroup_current = [
        s["cgroup_current_mib"] for s in samples if s["cgroup_current_mib"] is not None
    ]
    sandboxes = cfg.sandboxes_override
    if sandboxes is None:
        counts = [s["cgroups"] for s in samples if s["cgroups"] is not None]
        sandboxes = max(counts) if counts else 0
    oversell = sandboxes * cfg.cap_mib / mem_total if mem_total and sandboxes else None
    vmstat_first = next(
        (s["vmstat_oom_kill"] for s in samples if s["vmstat_oom_kill"] is not None),
        None,
    )
    vmstat_last = next(
        (
            s["vmstat_oom_kill"]
            for s in reversed(samples)
            if s["vmstat_oom_kill"] is not None
        ),
        None,
    )
    system_oom_delta = (
        None
        if vmstat_first is None or vmstat_last is None
        else max(0, vmstat_last - vmstat_first)
    )
    cgroup_oom_delta = samples[-1]["cgroup_oom_kill_accumulated"]
    # /proc/vmstat counts every OOM kill, including cgroup-contained guest
    # failures; derive the node-level share by subtracting the fleet cgroup
    # delta (floored at zero, since some kills may sit outside the sampled
    # cgroup root).
    host_oom_delta = (
        None
        if system_oom_delta is None
        else max(0, system_oom_delta - (cgroup_oom_delta or 0))
    )
    avail_pct = [100.0 * value / mem_total for value in mem_avail]
    psi_some = [
        s["psi_cpu_some_avg60"] for s in samples if s["psi_cpu_some_avg60"] is not None
    ]
    psi_full = [
        s["psi_cpu_full_avg60"] for s in samples if s["psi_cpu_full_avg60"] is not None
    ]
    psi_mem = [
        s["psi_mem_some_avg60"] for s in samples if s["psi_mem_some_avg60"] is not None
    ]
    violations = []
    if system_oom_delta is None:
        violations.append("vmstat_oom_kill_unavailable")
    elif system_oom_delta > 0 or cgroup_oom_delta:
        # vmstat is read before the cgroup files, so a kill that lands in that
        # window shows up only in the fleet delta; either observed delta is
        # an OOM.
        violations.append("oom_kills")
    if min(avail_pct) <= 0.0:
        violations.append("memory_exhausted")
    resident_mean = (
        sum(cgroup_current) / len(cgroup_current) if cgroup_current else None
    )
    # Per-sandbox cost is averaged over each sample's live cgroup count, so
    # fleet churn does not dilute it.
    per_sandbox_ratios = [
        s["cgroup_current_mib"] / s["cgroups"]
        for s in samples
        if s["cgroup_current_mib"] is not None and s["cgroups"]
    ]
    per_sandbox_mean = (
        sum(per_sandbox_ratios) / len(per_sandbox_ratios)
        if per_sandbox_ratios
        else None
    )
    return {
        "type": "summary",
        "backend": "host",
        "sandboxes": sandboxes,
        "cap_mib": cfg.cap_mib,
        "oversell_ratio": None if oversell is None else round(oversell, 4),
        "samples": len(samples),
        "duration_s": cfg.duration_s,
        "interval_s": cfg.interval_s,
        "resident_mib": {
            "mean_total": None if resident_mean is None else round(resident_mean, 3),
            "max_total": None if not cgroup_current else round(max(cgroup_current), 3),
            "mean_per_sandbox": None
            if per_sandbox_mean is None
            else round(per_sandbox_mean, 3),
        },
        "mem_available_pct": {
            "min": round(min(avail_pct), 4),
            "mean": round(sum(avail_pct) / len(avail_pct), 4),
        },
        "psi_max_pct": {
            "cpu_some": max(psi_some) if psi_some else None,
            "cpu_full": max(psi_full) if psi_full else None,
            "mem_some": max(psi_mem) if psi_mem else None,
        },
        "events": {
            "system_oom_kills_delta": system_oom_delta,
            "host_oom_kills_delta": host_oom_delta,
            "cgroup_oom_kills_delta": cgroup_oom_delta,
            "host_oom_method": "system_oom_kills_delta - cgroup_oom_kills_delta, floored at 0",
        },
        "verdict": {"ok": not violations, "violations": violations},
    }


# ---------------------------------------------------------------------------
# Reporting
# ---------------------------------------------------------------------------


def node_line(cfg: SimConfig) -> str:
    return f"- node: {cfg.node_memory_mib:.0f} MiB memory, {cfg.platform_reserved_mib:.0f} MiB platform reserve, {cfg.physical_cores} physical cores"


def horizon_line(cfg: SimConfig) -> str:
    cadence = (
        f"create rate {cfg.create_rate:g}/s"
        if cfg.create_rate
        else "instant fleet build"
    )
    return (
        f"- horizon: {cfg.duration_s:g} s at {cfg.interval_s:g} s intervals; "
        f"seed {cfg.seed}; {cadence}"
    )


def policy_line(cfg: SimConfig) -> str:
    return (
        f"- policy: red line {cfg.red_line_enter_pct:g}%/{cfg.red_line_exit_pct:g}%, "
        f"sigma-caps invariant {cfg.invariant_pct:g}%, tighten margin {cfg.tighten_margin_mib:g} MiB, "
        f"{'throttle-first' if cfg.throttle_first else 'inflate-first'} ordering, "
        f"deflate_on_oom {'on' if cfg.deflate_on_oom else 'off'}, "
        f"floor guarantee {'on' if cfg.floor_guarantee else 'off'}, "
        f"host OOM reserve {host_oom_reserve(cfg):g} MiB"
    )


def provenance_lines() -> list[str]:
    return [f"- {GUIDE_NOTE}", f"- proposal: OSEP-0033 {OSEP_0033_URL}"]


def matrix_meta_lines(base: SimConfig, rows: list, note: str) -> list[str]:
    """Report header for scenarios whose rows override the base config.

    Node shape and red-line bands are shared with the base run; everything
    the rows override (cap, floor, mix, tighten margin, ordering, horizon,
    OOM reserve) is rendered from the executed rows or named in the note, so
    the header cannot contradict the per-row summaries it introduces.
    """
    horizons = sorted({row.config.duration_s for row in rows})
    if len(horizons) == 1:
        horizon = f"- horizon: {horizons[0]:g} s at {base.interval_s:g} s intervals; seed {base.seed}"
    else:
        listed = ", ".join(f"{duration:g} s" for duration in horizons)
        horizon = (
            f"- horizon: per row ({listed}) at {base.interval_s:g} s intervals; "
            f"seed {base.seed}"
        )
    return [
        "- backend: sim (modeled projection, not a measurement)",
        f"- scenario: {base.scenario}",
        node_line(base),
        note,
        horizon,
        f"- policy: host OOM reserve {host_oom_reserve(rows[0].config):g} MiB; "
        "cap, mix, tighten margin, and ordering are per row (table columns "
        "and the JSON output)",
        *provenance_lines(),
    ]


def sim_meta_lines(cfg: SimConfig) -> list[str]:
    return [
        "- backend: sim (modeled projection, not a measurement)",
        f"- scenario: {cfg.scenario}",
        node_line(cfg),
        f"- sandbox: cap {cfg.cap_mib:.0f} MiB, floor {cfg.floor_mib:.0f} MiB, snapshot {cfg.snapshot_mib_per_sandbox:.0f} MiB",
        f"- workload mix: {format_mix(cfg.mix)}",
        horizon_line(cfg),
        policy_line(cfg),
        *provenance_lines(),
    ]


DENSITY_COLUMNS = "| oversell req/placed | sandboxes | mean resident MiB/sandbox | peak resident % | MemAvailable min % | PSI cpu some/full max % | red-line events | tighten actions/skips | guest OOM | host OOM | deflate saves | storage GiB | verdict |"


def density_table_row(cfg: SimConfig, result: TierResult) -> str:
    summary = result.to_summary(cfg, cfg.scenario)
    resident_value = summary["resident_mib"]["mean_per_sandbox"]
    resident = "n/a" if resident_value is None else f"{resident_value:.1f}"
    verdict = (
        "pass"
        if summary["verdict"]["ok"]
        else "fail: " + ",".join(summary["verdict"]["violations"])
    )
    storage = summary["storage_gib"]["total"]
    return (
        f"| {summary['requested_oversell_ratio']:.2f}x/{summary['placed_oversell_ratio']:.2f}x | "
        f"{result.placed_sandboxes} | "
        f"{resident} | {summary['peak_resident_pct']:.1f} | "
        f"{summary['mem_available_pct']['min']:.1f} | "
        f"{summary['psi_max_pct']['cpu_some']:.1f}/{summary['psi_max_pct']['cpu_full']:.1f} | "
        f"{result.counters.redline_events} | "
        f"{result.counters.tighten_actions}/{result.counters.tighten_skips} | "
        f"{result.counters.guest_oom_kills} | {result.counters.host_oom_kills} | "
        f"{result.counters.deflate_saves} | {storage:.1f} | {verdict} |"
    )


def render_density_report(cfg: SimConfig, results: list[TierResult]) -> str:
    lines = ["# Density tradeoff report", ""]
    lines.extend(sim_meta_lines(cfg))
    lines.extend(
        [
            "",
            "Resident, MemAvailable, and PSI columns are modeled projections; the calibration and its provenance are documented in the benchmark guide.",
            "",
        ]
    )
    for posture in cfg.postures:
        rows = [r for r in results if r.posture == posture]
        if not rows:
            continue
        label = (
            "red line only, no invariant"
            if posture == "redline"
            else "elastic floor/cap tier + sigma-caps invariant"
        )
        lines.append(f"## Posture: {posture} ({label})")
        lines.append("")
        lines.append(DENSITY_COLUMNS)
        lines.append(
            "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|:---|"
        )
        lines.extend(density_table_row(cfg, result) for result in rows)
        lines.append("")
    return "\n".join(lines) + "\n"


def render_matrix_report(
    title: str, meta: list[str], columns: str, separator: str, rows: list[str]
) -> str:
    lines = [f"# {title}", ""]
    lines.extend(meta)
    lines.extend(["", columns, separator])
    lines.extend(rows)
    return "\n".join(lines) + "\n"


def render_host_report(summary: dict, cfg: HostConfig) -> str:
    lines = [
        "# Density tradeoff report",
        "",
        "- backend: host (read-only measurement; it creates nothing)",
        f"- node: {summary['samples']} samples over {summary['duration_s']:g} s at {cfg.interval_s:g} s intervals",
        f"- fleet: {summary['sandboxes']} sandboxes, declared cap {cfg.cap_mib:.0f} MiB each"
        + ("" if cfg.cgroup_root else " (count supplied; no cgroup root given)"),
        f"- {GUIDE_NOTE}",
        f"- proposal: OSEP-0033 {OSEP_0033_URL}",
        "",
        "| oversell | sandboxes | mean resident MiB/sandbox | MemAvailable min % | PSI max % (cpu some/full; mem some) | system OOM delta | host OOM delta | cgroup OOM delta | verdict |",
        "|---|---:|---:|---:|---:|---:|---:|---:|:---|",
    ]
    resident = summary["resident_mib"]["mean_per_sandbox"]
    psi_some = summary["psi_max_pct"]["cpu_some"]
    psi_full = summary["psi_max_pct"]["cpu_full"]
    verdict = (
        "pass"
        if summary["verdict"]["ok"]
        else "fail: " + ",".join(summary["verdict"]["violations"])
    )
    oversell = summary["oversell_ratio"]
    events = summary["events"]
    system_oom = events.get("system_oom_kills_delta")
    host_oom = events.get("host_oom_kills_delta")
    cgroup_oom = events.get("cgroup_oom_kills_delta")
    per_sandbox = "n/a" if resident is None else f"{resident:.1f}"
    psi_mem = summary["psi_max_pct"].get("mem_some")
    psi_pair = (
        "n/a"
        if psi_some is None or psi_full is None
        else f"{psi_some:.1f}/{psi_full:.1f}"
    )
    if psi_mem is not None:
        psi_pair += f"; {psi_mem:.1f}"
    lines.append(
        f"| {'n/a' if oversell is None else f'{oversell:.2f}x'} | {summary['sandboxes']} | "
        f"{per_sandbox} | {summary['mem_available_pct']['min']:.1f} | {psi_pair} | "
        f"{'n/a' if system_oom is None else system_oom} | "
        f"{'n/a' if host_oom is None else host_oom} | "
        f"{'n/a' if cgroup_oom is None else cgroup_oom} | {verdict} |"
    )
    return "\n".join(lines) + "\n"


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def build_sim_config(args) -> SimConfig:
    snapshot = args.snapshot_mib_per_sandbox
    if snapshot is None:
        snapshot = args.cap_mib
    # "both" expands to the two real postures; the third value is CLI sugar.
    postures = POSTURES[:-1] if args.posture == "both" else (args.posture,)
    return SimConfig(
        node_memory_mib=args.node_memory_mib,
        platform_reserved_mib=args.platform_reserved_mib,
        physical_cores=args.physical_cores,
        cap_mib=args.cap_mib,
        floor_mib=args.floor_mib,
        interval_s=args.interval,
        duration_s=args.duration,
        seed=args.seed,
        create_rate=args.create_rate,
        host_oom_reserve_mib=args.host_oom_reserve_mib,
        mix=args.mix,
        snapshot_mib_per_sandbox=snapshot,
        storage_gib=args.storage_gib,
        storage_headroom_pct=args.storage_headroom_pct,
        postures=postures,
        scenario=args.scenario,
        red_line_enter_pct=args.red_line_enter_pct,
        red_line_exit_pct=args.red_line_exit_pct,
        invariant_pct=args.invariant_pct,
        tighten_margin_mib=args.tighten_margin_mib,
        oom_margin_threshold_mib=args.oom_margin_threshold_mib,
        throttle_first=not args.inflate_first,
        deflate_on_oom=not args.no_deflate_on_oom,
        floor_guarantee=not args.no_floor_guarantee,
        fill_ratio=args.fill_ratio,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="density_tradeoff.py",
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("--backend", choices=BACKENDS, default="sim")
    parser.add_argument("--scenario", choices=SCENARIOS, default="density")
    parser.add_argument("--posture", choices=POSTURES, default="both")
    parser.add_argument(
        "--tiers",
        default="0.5,1,2,4",
        help="Oversell ratios (sum of declared caps / node memory) to sweep",
    )
    parser.add_argument(
        "--node-memory-mib",
        type=float,
        default=515745.0,
        help="Node memory; default is the evidence-class node (~504 GiB)",
    )
    parser.add_argument(
        "--platform-reserved-mib",
        type=float,
        default=112640.0,
        help="Non-sandbox baseline charged against MemAvailable and the invariant",
    )
    parser.add_argument("--physical-cores", type=int, default=128)
    parser.add_argument("--cap-mib", type=float, default=512.0)
    parser.add_argument("--floor-mib", type=float, default=256.0)
    parser.add_argument("--mix", default="idle=0.8,sparse=0.1,bursty=0.1")
    parser.add_argument("--duration", type=float, default=300.0)
    parser.add_argument("--interval", type=float, default=1.0)
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument(
        "--create-rate",
        type=float,
        default=0.0,
        help="Sandboxes created per second; 0 builds the fleet instantly",
    )
    parser.add_argument("--snapshot-mib-per-sandbox", type=float, default=None)
    parser.add_argument("--storage-gib", type=float, default=3500.0)
    parser.add_argument("--storage-headroom-pct", type=float, default=10.0)
    parser.add_argument("--red-line-enter-pct", type=float, default=15.0)
    parser.add_argument("--red-line-exit-pct", type=float, default=18.0)
    parser.add_argument("--invariant-pct", type=float, default=80.0)
    parser.add_argument("--tighten-margin-mib", type=float, default=512.0)
    parser.add_argument("--oom-margin-threshold-mib", type=float, default=512.0)
    parser.add_argument(
        "--host-oom-reserve-mib",
        type=float,
        default=None,
        help=(
            "Last-resort eviction threshold; default scales to 0.8%% of node "
            "memory, floored at 256 MiB and capped at 90%% of the red-line "
            "enter band"
        ),
    )
    parser.add_argument(
        "--fill-ratio",
        type=float,
        default=1.0,
        help="How far dense profiles fill their declared cap",
    )
    parser.add_argument("--inflate-first", action="store_true")
    parser.add_argument("--no-deflate-on-oom", action="store_true")
    parser.add_argument("--no-floor-guarantee", action="store_true")
    parser.add_argument("--out-jsonl", type=Path)
    parser.add_argument("--out-summary", type=Path)
    parser.add_argument(
        "--report", type=Path, help="Write the markdown report here too"
    )
    # host backend
    parser.add_argument("--proc-root", type=Path, default=Path("/proc"))
    parser.add_argument(
        "--cgroup-root",
        type=Path,
        help="Parent cgroup whose child directories are sandboxes (cgroup v2)",
    )
    parser.add_argument(
        "--sandboxes",
        type=int,
        help=(
            "Fleet size for the host backend when --cgroup-root is not given "
            "(nonnegative)"
        ),
    )
    return parser


def arguments(argv=None) -> argparse.Namespace:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        args.mix = parse_mix(args.mix)
    except ValueError as exc:
        parser.error(str(exc))
    if args.cap_mib <= 0:
        parser.error("cap_mib must be positive")
    if args.backend == "sim":
        # The floor is an elastic-tier simulator concept; the host sampler
        # never consumes it. It must also cover the modeled boot footprint,
        # or a floor-capped guest would be born above its effective cap.
        footprint = SimConfig.idle_anon_mib + SimConfig.image_file_mib
        if args.floor_mib < footprint or args.floor_mib >= args.cap_mib:
            parser.error(
                f"Require floor_mib >= {footprint:g} MiB (the modeled boot "
                "footprint) and < cap_mib"
            )
    if args.duration <= 0 or args.interval <= 0:
        parser.error("Duration and interval must be positive")
    if (
        args.node_memory_mib <= 0
        or args.platform_reserved_mib < 0
        or args.platform_reserved_mib >= args.node_memory_mib
    ):
        parser.error(
            "Node memory must be positive and the platform reserve must be "
            "nonnegative and smaller than node memory"
        )
    if args.scenario == "tighten-margins" and args.interval > (
        MATRIX_INFLATE_FIRST_TRIGGER_S
    ):
        parser.error(
            "The tighten matrix's forced triggers need --interval <= "
            f"{MATRIX_INFLATE_FIRST_TRIGGER_S:g} s"
        )
    if args.physical_cores < 1:
        parser.error("Physical cores must be positive")
    if args.create_rate < 0:
        parser.error("Create rate must be nonnegative")
    if args.snapshot_mib_per_sandbox is not None and (
        not math.isfinite(args.snapshot_mib_per_sandbox)
        or args.snapshot_mib_per_sandbox < 0
    ):
        parser.error("Snapshot size must be finite and nonnegative")
    if args.backend == "sim":
        try:
            args.ratios = [
                float(item) for item in args.tiers.split(",") if item.strip()
            ]
        except ValueError:
            parser.error("--tiers must be a comma-separated list of numbers")
        if not args.ratios or any(ratio <= 0 for ratio in args.ratios):
            parser.error("--tiers ratios must be positive")
        for name in ("red_line_enter_pct", "red_line_exit_pct", "invariant_pct"):
            value = getattr(args, name)
            if not 0 < value <= 100:
                parser.error(f"--{name.replace('_', '-')} must be in (0, 100]")
        if not args.red_line_enter_pct < args.red_line_exit_pct:
            parser.error("The red-line exit threshold must exceed the enter threshold")
        if args.tighten_margin_mib < 0:
            parser.error("Tighten margin must be nonnegative")
    else:
        if args.scenario != "density":
            parser.error(
                "The host backend samples a running fleet; use --scenario density"
            )
        if args.cgroup_root is None and args.sandboxes is None:
            parser.error("Host backend needs --cgroup-root or --sandboxes")
        if args.sandboxes is not None and args.sandboxes < 0:
            parser.error("--sandboxes must be nonnegative")
        if args.cgroup_root is not None and args.sandboxes is not None:
            parser.error(
                "--sandboxes is the fleet-size override for runs without "
                "--cgroup-root; the cgroup listing already counts sandboxes"
            )
        if args.cgroup_root is not None and not args.cgroup_root.is_dir():
            parser.error(f"cgroup root {args.cgroup_root} is not a directory")
        if not (args.proc_root / "meminfo").is_file():
            parser.error(f"{args.proc_root}/meminfo not found")
    return args


def main(argv=None) -> int:
    args = arguments(argv)
    if args.backend == "host":
        host_cfg = HostConfig(
            proc_root=args.proc_root,
            cgroup_root=args.cgroup_root,
            duration_s=args.duration,
            interval_s=args.interval,
            cap_mib=args.cap_mib,
            sandboxes_override=args.sandboxes,
        )
        samples = collect_host_samples(host_cfg)
        summary = summarize_host_samples(samples, host_cfg)
        report = render_host_report(summary, host_cfg)
        if args.out_jsonl:
            with args.out_jsonl.open("w", encoding="utf-8") as handle:
                for sample in samples:
                    handle.write(json.dumps(sample) + "\n")
                handle.write(json.dumps(summary) + "\n")
        if args.out_summary:
            # Same array schema as the simulator branches, per the guide.
            args.out_summary.write_text(
                json.dumps([summary], indent=2) + "\n", encoding="utf-8"
            )
        if args.report:
            args.report.write_text(report, encoding="utf-8")
        print(report)
        return 0

    cfg_sim = build_sim_config(args)
    summaries: list[dict] = []
    samples_by_tier: list[dict] = []
    if cfg_sim.scenario == "density":
        results = run_density_scenario(cfg_sim, args.ratios)
        report = render_density_report(cfg_sim, results)
        summaries = [r.to_summary(cfg_sim, "density") for r in results]
        for result, summary in zip(results, summaries, strict=True):
            tier_tag = {
                "posture": result.posture,
                "requested_oversell_ratio": summary["requested_oversell_ratio"],
            }
            for sample in result.samples:
                samples_by_tier.append({**tier_tag, **sample})
            samples_by_tier.append(summary)
    elif cfg_sim.scenario == "tighten-margins":
        rows = run_tighten_matrix(cfg_sim)
        summaries = [row.to_row() for row in rows]
        table_rows = [
            f"| {row['case']} | {row['cap_mib']:.0f} | {row['tighten_margin_mib']:.0f} | {row['ordering']} | "
            f"{row['tighten_actions']}/{row['tighten_skips']} | {row['guest_oom_kills']} | {row['host_oom_kills']} | "
            f"{'pass' if row['verdict']['ok'] else 'fail'} |"
            for row in summaries
        ]
        report = render_matrix_report(
            "Tighten-ordering matrix (modeled)",
            matrix_meta_lines(
                cfg_sim,
                rows,
                "- rows override cap, tighten margin, ordering, mix, fill ratio, horizon, and the OOM reserve; every row runs a dense workload that fills 70% of its cap, with a synthetic red-line trigger pinned to elapsed time (25 s, or 10 s for the inflate-first case) and its release 20 s later; every row's exact configuration and derived tick are in the JSON output",
            )
            + [
                "",
                "Expected outcomes follow the OSEP-0033 Test Plan (E8): safe margin holds, the known-bad margin keeps the failure visible, an infeasible floor skips, and inflate-before-throttle OOMs.",
            ],
            "| case | cap MiB | margin MiB | ordering | actions/skips | guest OOM | host OOM | verdict |",
            "|---|---:|---:|---|---:|---:|---:|:---|",
            table_rows,
        )
        samples_by_tier = []
        for row in rows:
            ordering = "throttle-first" if row.throttle_first else "inflate-first"
            tag = {
                "case": row.case,
                "cap_mib": row.cap_mib,
                "tighten_margin_mib": row.margin_mib,
                "ordering": ordering,
            }
            for sample in row.result.samples:
                samples_by_tier.append({**tag, **sample})
        samples_by_tier.extend(summaries)
    else:
        rescue_cfg, rows, results = run_deflate_rescue(cfg_sim)
        summaries = rows
        table_rows = [
            f"| deflate_on_oom={row['deflate_on_oom']} | {row['sandboxes']} | {row['deflate_saves']} | "
            f"{row['guest_oom_kills']} | {row['host_oom_kills']} | {'pass' if row['verdict']['ok'] else 'fail'} |"
            for row in rows
        ]
        report = render_matrix_report(
            "deflate_on_oom rescue (modeled)",
            sim_meta_lines(rescue_cfg)
            + [
                "",
                "A burst beyond the balloon-reduced effective cap must be absorbed by automatic deflation (E5); with the escape hatch off the guest process OOMs.",
            ],
            "| case | sandboxes | deflate saves | guest OOM | host OOM | verdict |",
            "|---|---:|---:|---:|---:|:---|",
            table_rows,
        )
        samples_by_tier = []
        for deflate_on, result in zip((True, False), results, strict=True):
            tag = {"case": "deflate-rescue", "deflate_on_oom": deflate_on}
            for sample in result.samples:
                samples_by_tier.append({**tag, **sample})
        samples_by_tier.extend(summaries)

    if args.out_jsonl:
        with args.out_jsonl.open("w", encoding="utf-8") as handle:
            for record in samples_by_tier:
                handle.write(json.dumps(record) + "\n")
    if args.out_summary:
        args.out_summary.write_text(
            json.dumps(summaries, indent=2) + "\n", encoding="utf-8"
        )
    if args.report:
        args.report.write_text(report, encoding="utf-8")
    print(report)
    return 0


if __name__ == "__main__":
    sys.exit(main())
