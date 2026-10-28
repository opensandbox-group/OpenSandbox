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

"""Sweeper gate-order tests with in-memory collaborators."""

from datetime import datetime, timedelta, timezone
import logging
from types import SimpleNamespace
from typing import Optional

import pytest
from redis.exceptions import RedisError

from opensandbox_server.config import (
    GatewayConfig,
    GatewayRouteModeConfig,
    IngressConfig,
    SecureAccessConfig,
    SecureAccessKey,
)
from opensandbox_server.integrations.idle_pause.activity import (
    ActivityRegistry,
    unknown_tombstone_key,
)
from opensandbox_server.integrations.idle_pause.executor import PauseOutcome
from opensandbox_server.integrations.idle_pause.inventory import SandboxCandidate
from opensandbox_server.integrations.idle_pause.locking import PauseLockManager
from opensandbox_server.integrations.idle_pause.policy import (
    ActivityProbe,
    IdlePolicy,
    validate_idle_policy,
)
from opensandbox_server.integrations.idle_pause.probe import (
    PROBE_OUTCOME_ACTIVE,
    PROBE_OUTCOME_ERROR,
    PROBE_OUTCOME_NOT_ACTIVE,
)
from opensandbox_server.integrations.idle_pause.sweeper import (
    IdlePauseSweeper,
    _build_activity_prober,
)
from tests.test_idle_pause_activity import FakeAsyncRedis

NOW_MS = 1_000_000_000_000
X = 300


def _policy(**kwargs) -> IdlePolicy:
    return validate_idle_policy(idle_timeout_seconds=X, on_idle="pause", **kwargs)


def _candidate(
    sandbox_id: str = "fsb_1",
    *,
    state: str = "Running",
    policy: Optional[IdlePolicy] = None,
    expires_in: Optional[int] = 3600,
) -> SandboxCandidate:
    expires_at = (
        datetime.now(timezone.utc) + timedelta(seconds=expires_in) if expires_in is not None else None
    )
    return SandboxCandidate(
        sandbox_id=sandbox_id,
        namespace="ns",
        uid="uid-1",
        generation=7,
        state=state,
        expires_at=expires_at,
        policy=policy,
    )


class FakeInventory:
    def __init__(self, candidates: list[SandboxCandidate]):
        self.candidates = candidates

    def snapshot(self, namespace: str) -> list[SandboxCandidate]:
        return list(self.candidates)


class FakeExecutor:
    def __init__(self, outcome: PauseOutcome = PauseOutcome.ACCEPTED):
        self.outcome = outcome
        self.calls: list[SandboxCandidate] = []

    def pause(self, candidate: SandboxCandidate) -> PauseOutcome:
        self.calls.append(candidate)
        return self.outcome


def _sweeper(
    candidates: list[SandboxCandidate],
    redis_client: FakeAsyncRedis,
    executor: FakeExecutor,
    *,
    clock_now: int = NOW_MS,
    namespaces: Optional[list[str]] = None,
    prober: Optional[object] = None,
) -> IdlePauseSweeper:
    return IdlePauseSweeper(
        namespace_resolver=lambda: namespaces or ["ns"],
        inventory=FakeInventory(candidates),
        activity=ActivityRegistry(redis_client, activity_ttl_seconds=1800),  # type: ignore[arg-type]
        locks=PauseLockManager(redis_client, lock_ttl_seconds=30),  # type: ignore[arg-type]
        executor=executor,  # type: ignore[arg-type]
        sweep_interval_seconds=15,
        clock=lambda: clock_now,
        prober=prober,  # type: ignore[arg-type]
    )


@pytest.mark.asyncio
async def test_idle_by_value_issues_pause_with_idempotent_request_id():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)

    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    stats = await sweeper.sweep_once()

    assert len(executor.calls) == 1
    assert executor.calls[0].uid == candidate.uid
    assert executor.calls[0].generation == candidate.generation
    assert stats.paused == 1
    assert stats.gated == 0


@pytest.mark.asyncio
async def test_fresh_activity_never_pauses():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - 1000
    )
    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.paused == 0


@pytest.mark.asyncio
async def test_missing_key_creates_tombstone_then_pauses_once_aged():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)

    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.tombstoned == 1
    assert redis.store[unknown_tombstone_key(candidate.sandbox_id)] == str(NOW_MS)
    assert redis.ttls[unknown_tombstone_key(candidate.sandbox_id)] == 1800 + 15

    # Second sweep, tombstone younger than X: still unknown.
    sweeper_later = _sweeper(
        [candidate], redis, executor, clock_now=NOW_MS + (X - 10) * 1000
    )
    await sweeper_later.sweep_once()
    assert executor.calls == []

    # Tombstone aged >= X: idle, pause proceeds.
    sweeper_final = _sweeper(
        [candidate], redis, executor, clock_now=NOW_MS + (X + 1) * 1000
    )
    stats = await sweeper_final.sweep_once()
    assert len(executor.calls) == 1
    assert stats.paused == 1


@pytest.mark.asyncio
async def test_wake_only_policy_is_never_auto_paused():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=validate_idle_policy(idle_timeout_seconds=None))
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)
    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.opted_in == 0


@pytest.mark.asyncio
async def test_non_running_and_expired_are_skipped():
    redis = FakeAsyncRedis()
    paused = _candidate("fsb_paused", state="Paused", policy=_policy())
    resuming = _candidate("fsb_resuming", state="Resuming", policy=_policy())
    expired = _candidate("fsb_expired", policy=_policy(), expires_in=-1)
    executor = FakeExecutor()
    sweeper = _sweeper([paused, resuming, expired], redis, executor)
    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.paused == 0
    assert stats.opted_in == 3


@pytest.mark.asyncio
async def test_lock_contention_gates_without_pause():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    await PauseLockManager(redis, lock_ttl_seconds=30).try_acquire(  # type: ignore[arg-type]
        candidate.sandbox_id, "other-replica"
    )
    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.gated == 1


@pytest.mark.asyncio
async def test_activity_landing_between_read_and_lock_drops_candidate():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    registry = FakeSlippingRegistry(redis, slip_at_read=True)
    sweeper = IdlePauseSweeper(
        namespace_resolver=lambda: ["ns"],
        inventory=FakeInventory([candidate]),
        activity=registry,  # type: ignore[arg-type]
        locks=PauseLockManager(redis, lock_ttl_seconds=30),  # type: ignore[arg-type]
        executor=executor,  # type: ignore[arg-type]
        sweep_interval_seconds=15,
        clock=lambda: NOW_MS,
    )
    await registry.record(candidate.sandbox_id, now_ms=NOW_MS - (X + 5) * 1000)
    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.gated >= 1


class FakeSlippingRegistry:
    """Re-verify read observes a newer write (a request landed meanwhile)."""

    def __init__(self, redis: FakeAsyncRedis, slip_at_read: bool):
        self._inner = ActivityRegistry(redis, activity_ttl_seconds=1800)  # type: ignore[arg-type]
        self._slip = slip_at_read
        self._slipped = False

    async def record(self, *args, **kwargs):
        await self._inner.record(*args, **kwargs)

    async def ping(self):
        await self._inner.ping()

    async def read_many_with_tombstones(self, ids):
        return await self._inner.read_many_with_tombstones(ids)

    async def read(self, sandbox_id: str):
        value = await self._inner.read(sandbox_id)
        if self._slip and not self._slipped:
            self._slipped = True
            return (value or 0) + 1  # a request landed meanwhile
        return value

    async def ensure_tombstone(self, *args, **kwargs):
        await self._inner.ensure_tombstone(*args, **kwargs)

    async def clear_tombstone(self, sandbox_id: str):
        await self._inner.clear_tombstone(sandbox_id)


@pytest.mark.asyncio
async def test_declared_probe_gates_pause_until_probe_pipeline_lands():
    redis = FakeAsyncRedis()
    probe = ActivityProbe(port=8080, path="/active", timeout_seconds=2)
    candidate = _candidate(policy=_policy(active_probe=probe))
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.gated == 1


class _StubProber:
    """Scripted prober; optionally records activity while probing."""

    def __init__(self, verdicts: list[tuple[str, Optional[str]]], record_during_probe=None):
        self.verdicts = list(verdicts)
        self.probed: list[SandboxCandidate] = []
        self.record_during_probe = record_during_probe

    async def probe(self, candidate: SandboxCandidate):
        self.probed.append(candidate)
        if self.record_during_probe is not None:
            await self.record_during_probe(candidate)
        outcome, message = self.verdicts.pop(0)
        from opensandbox_server.integrations.idle_pause.probe import ProbeVerdict

        return ProbeVerdict(outcome, message)

    async def aclose(self) -> None:
        pass


def _probe_candidate() -> SandboxCandidate:
    return _candidate(policy=_policy(active_probe=ActivityProbe(port=8080, path="/active")))


@pytest.mark.asyncio
async def test_probe_active_answer_records_activity_and_drops_candidate():
    redis = FakeAsyncRedis()
    candidate = _probe_candidate()
    executor = FakeExecutor()
    registry = ActivityRegistry(redis, activity_ttl_seconds=1800)  # type: ignore[arg-type]
    await registry.record(candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000)
    prober = _StubProber(
        [(PROBE_OUTCOME_ACTIVE, None)],
        record_during_probe=lambda _c: registry.record(
            candidate.sandbox_id, now_ms=NOW_MS
        ),
    )
    sweeper = _sweeper([candidate], redis, executor, prober=prober)

    stats = await sweeper.sweep_once()
    await sweeper.stop()

    assert executor.calls == []
    assert stats.gated == 1
    # The busy answer IS activity: the key now carries the fresh observation.
    assert await registry.read(candidate.sandbox_id) == NOW_MS


@pytest.mark.asyncio
async def test_probe_not_active_answer_lets_pause_proceed():
    redis = FakeAsyncRedis()
    candidate = _probe_candidate()
    executor = FakeExecutor()
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    prober = _StubProber([(PROBE_OUTCOME_NOT_ACTIVE, "cron loop idle")])
    sweeper = _sweeper([candidate], redis, executor, prober=prober)

    stats = await sweeper.sweep_once()
    await sweeper.stop()

    assert len(executor.calls) == 1
    assert stats.paused == 1
    assert stats.gated == 0


@pytest.mark.asyncio
async def test_probe_error_drops_candidate_without_pause():
    redis = FakeAsyncRedis()
    candidate = _probe_candidate()
    executor = FakeExecutor()
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    prober = _StubProber([(PROBE_OUTCOME_ERROR, None)])
    sweeper = _sweeper([candidate], redis, executor, prober=prober)

    stats = await sweeper.sweep_once()
    await sweeper.stop()

    assert executor.calls == []
    assert stats.gated == 1


@pytest.mark.asyncio
async def test_request_landing_during_probe_drops_candidate():
    redis = FakeAsyncRedis()
    candidate = _probe_candidate()
    executor = FakeExecutor()
    registry = ActivityRegistry(redis, activity_ttl_seconds=1800)  # type: ignore[arg-type]
    await registry.record(candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000)
    prober = _StubProber(
        [(PROBE_OUTCOME_NOT_ACTIVE, None)],
        record_during_probe=lambda _c: registry.record(
            candidate.sandbox_id, now_ms=NOW_MS
        ),
    )
    sweeper = _sweeper([candidate], redis, executor, prober=prober)

    stats = await sweeper.sweep_once()
    await sweeper.stop()

    # The second re-verify catches the observation that landed while the
    # probe ran: no pause, next sweep re-evaluates.
    assert executor.calls == []
    assert stats.gated == 1


@pytest.mark.asyncio
async def test_pause_state_changed_counts_as_gated_not_failed():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor(outcome=PauseOutcome.STATE_CHANGED)
    sweeper = _sweeper([candidate], redis, executor)
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    stats = await sweeper.sweep_once()
    assert stats.gated == 1
    assert stats.failed == 0


@pytest.mark.asyncio
async def test_redis_unreachable_skips_sweep_entirely():
    redis = FakeAsyncRedis(fail=True)
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)
    stats = await sweeper.sweep_once()
    assert executor.calls == []
    assert stats.candidates == 0


@pytest.mark.asyncio
async def test_pause_issued_audit_line_carries_threshold_and_age():
    class Capture(logging.Handler):
        def __init__(self) -> None:
            super().__init__()
            self.records: list[logging.LogRecord] = []

        def emit(self, record: logging.LogRecord) -> None:
            self.records.append(record)

    # configure_logging sets the opensandbox_server logger propagate=False,
    # so records must be captured at the module logger itself.
    handler = Capture()
    sweeper_logger = logging.getLogger(
        "opensandbox_server.integrations.idle_pause.sweeper"
    )
    sweeper_logger.addHandler(handler)
    try:
        redis = FakeAsyncRedis()
        candidate = _candidate(policy=_policy())
        executor = FakeExecutor()
        sweeper = _sweeper([candidate], redis, executor)
        await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
            candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
        )
        await sweeper.sweep_once()
    finally:
        sweeper_logger.removeHandler(handler)

    records = [r for r in handler.records if getattr(r, "idlep_event", None) == "pause_issued"]
    assert len(records) == 1
    record = records[0]
    assert record.idlep_sandbox_id == candidate.sandbox_id  # type: ignore[attr-defined]
    assert record.idlep_threshold_seconds == X  # type: ignore[attr-defined]
    assert record.idlep_last_observation_age == "310.0s"  # type: ignore[attr-defined]
    assert record.idlep_tombstoned is False  # type: ignore[attr-defined]


class FailingAcquireLocks:
    """try_acquire raises, as when Redis dies mid-sweep."""

    def __init__(self, redis_client: FakeAsyncRedis):
        self._inner = PauseLockManager(redis_client, lock_ttl_seconds=30)  # type: ignore[arg-type]

    async def try_acquire(self, sandbox_id: str, owner: str) -> bool:
        raise RedisError("connection reset")

    async def release(self, sandbox_id: str, owner: str) -> None:
        await self._inner.release(sandbox_id, owner)


class FailingReleaseLocks:
    """release raises inside the sweeper's finally block."""

    async def try_acquire(self, sandbox_id: str, owner: str) -> bool:
        return True

    async def release(self, sandbox_id: str, owner: str) -> None:
        raise RedisError("connection reset")


@pytest.mark.asyncio
async def test_lock_acquire_redis_error_drops_candidate_without_breaking_sweep():
    redis = FakeAsyncRedis()
    first = _candidate("fsb_a", policy=_policy())
    second = _candidate("fsb_b", policy=_policy())
    executor = FakeExecutor()
    sweeper = IdlePauseSweeper(
        namespace_resolver=lambda: ["ns1", "ns2"],
        inventory=FakeInventory([first, second]),
        activity=ActivityRegistry(redis, activity_ttl_seconds=1800),  # type: ignore[arg-type]
        locks=FailingAcquireLocks(redis),  # type: ignore[arg-type]
        executor=executor,  # type: ignore[arg-type]
        sweep_interval_seconds=15,
        clock=lambda: NOW_MS,
    )
    for c in (first, second):
        await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
            c.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
        )
    stats = await sweeper.sweep_once()  # must not raise
    assert executor.calls == []
    assert stats.gated == 4  # both candidates, both namespaces


@pytest.mark.asyncio
async def test_lock_release_redis_error_does_not_mask_pause_outcome():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = IdlePauseSweeper(
        namespace_resolver=lambda: ["ns"],
        inventory=FakeInventory([candidate]),
        activity=ActivityRegistry(redis, activity_ttl_seconds=1800),  # type: ignore[arg-type]
        locks=FailingReleaseLocks(),  # type: ignore[arg-type]
        executor=executor,  # type: ignore[arg-type]
        sweep_interval_seconds=15,
        clock=lambda: NOW_MS,
    )
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    stats = await sweeper.sweep_once()
    assert stats.paused == 1


@pytest.mark.asyncio
async def test_namespace_resolver_is_reconsulted_every_sweep():
    redis = FakeAsyncRedis()
    tenant_b = _candidate("fsb_b", policy=_policy())
    tenant_b_ns = SandboxCandidate(
        sandbox_id=tenant_b.sandbox_id,
        namespace="ns_b",
        uid=tenant_b.uid,
        generation=tenant_b.generation,
        state=tenant_b.state,
        expires_at=tenant_b.expires_at,
        policy=tenant_b.policy,
    )

    class NamespaceFilteredInventory:
        def snapshot(self, namespace: str) -> list[SandboxCandidate]:
            return [tenant_b_ns] if namespace == "ns_b" else []

    executor = FakeExecutor()
    namespaces: list[list[str]] = [["ns_a"]]
    sweeper = IdlePauseSweeper(
        namespace_resolver=lambda: namespaces[0],
        inventory=NamespaceFilteredInventory(),
        activity=ActivityRegistry(redis, activity_ttl_seconds=1800),  # type: ignore[arg-type]
        locks=PauseLockManager(redis, lock_ttl_seconds=30),  # type: ignore[arg-type]
        executor=executor,  # type: ignore[arg-type]
        sweep_interval_seconds=15,
        clock=lambda: NOW_MS,
    )
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        tenant_b.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    stats_first = await sweeper.sweep_once()
    assert stats_first.candidates == 0  # tenant B not yet known
    namespaces[0] = ["ns_a", "ns_b"]  # tenant added at runtime
    stats_second = await sweeper.sweep_once()
    assert len(executor.calls) == 1
    assert stats_second.paused == 1


class OwnedClientRedis(FakeAsyncRedis):
    def __init__(self):
        super().__init__()
        self.closed = False

    async def aclose(self) -> None:
        self.closed = True


@pytest.mark.asyncio
async def test_start_owns_and_stops_client_it_created(monkeypatch):
    import opensandbox_server.integrations.idle_pause.sweeper as sweeper_module

    fake = OwnedClientRedis()
    monkeypatch.setattr(
        sweeper_module, "connect_idle_pause_redis_from_config", _async_returning(fake)
    )
    sweeper = await sweeper_module.start_idle_pause_sweeper(
        _idle_pause_config(), _fsb_service(), namespace_resolver=lambda: ["ns"]
    )
    assert sweeper is not None
    await sweeper.stop()
    assert fake.closed is True


@pytest.mark.asyncio
async def test_start_does_not_close_caller_supplied_client():
    import opensandbox_server.integrations.idle_pause.sweeper as sweeper_module

    fake = OwnedClientRedis()
    sweeper = await sweeper_module.start_idle_pause_sweeper(
        _idle_pause_config(),
        _fsb_service(),
        namespace_resolver=lambda: ["ns"],
        redis_client=fake,  # type: ignore[arg-type]
    )
    assert sweeper is not None
    await sweeper.stop()
    assert fake.closed is False


@pytest.mark.asyncio
async def test_start_degrades_on_malformed_dsn(monkeypatch):
    import opensandbox_server.integrations.idle_pause.sweeper as sweeper_module

    async def _raise(*args, **kwargs):
        raise ValueError("Redis URL must specify one of the supported schemes")

    monkeypatch.setattr(sweeper_module, "connect_idle_pause_redis_from_config", _raise)
    sweeper = await sweeper_module.start_idle_pause_sweeper(
        _idle_pause_config(), _fsb_service(), namespace_resolver=lambda: ["ns"]
    )
    assert sweeper is None


def _async_returning(value):
    async def _inner(*args, **kwargs):
        return value

    return _inner


def _idle_pause_config():
    from opensandbox_server.config import (
        AppConfig,
        AutomationConfig,
        IdlePauseConfig,
        IdlePauseRedisConfig,
        RuntimeConfig,
    )

    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="x"),
        automation=AutomationConfig(
            idle_pause=IdlePauseConfig(
                enabled=True,
                redis=IdlePauseRedisConfig(dsn="redis://127.0.0.1:6379/5"),
            )
        ),
    )


def _fsb_service():
    from opensandbox_server.config import AppConfig, RuntimeConfig
    from opensandbox_server.services.fast_sandbox.service import FastSandboxService

    return FastSandboxService(
        AppConfig(runtime=RuntimeConfig(type="kubernetes", execd_image="x"))
    )


class NoClearRegistry(FakeSlippingRegistry):
    """clear_tombstone raises (Redis hiccup confined to the clear path)."""

    async def clear_tombstone(self, sandbox_id: str):
        raise RedisError("read-only replica")


@pytest.mark.asyncio
async def test_activity_reappearance_clears_leftover_tombstone():
    """Missing-key period tombstoned, traffic recreated the key: the tombstone
    must not outlive the fresh observation (its age would fake idleness after
    a later eviction)."""
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)
    registry = ActivityRegistry(redis, activity_ttl_seconds=1800)  # type: ignore[arg-type]
    # Tombstone from an older missing-key window, aged >= X.
    await redis.set(unknown_tombstone_key(candidate.sandbox_id), NOW_MS - (X + 10) * 1000, ex=3600)
    # Fresh traffic recreated the activity key.
    await registry.record(candidate.sandbox_id, now_ms=NOW_MS - 1000)

    stats = await sweeper.sweep_once()

    assert unknown_tombstone_key(candidate.sandbox_id) not in redis.store
    assert executor.calls == []  # fresh activity: not idle
    assert stats.tombstoned == 0


@pytest.mark.asyncio
async def test_stale_activity_clears_tombstone_and_pauses():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = _sweeper([candidate], redis, executor)
    registry = ActivityRegistry(redis, activity_ttl_seconds=1800)  # type: ignore[arg-type]
    await redis.set(unknown_tombstone_key(candidate.sandbox_id), NOW_MS - (X + 10) * 1000, ex=3600)
    await registry.record(candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000)

    stats = await sweeper.sweep_once()

    assert unknown_tombstone_key(candidate.sandbox_id) not in redis.store
    assert len(executor.calls) == 1
    assert stats.paused == 1


@pytest.mark.asyncio
async def test_tombstone_clear_failure_is_fail_safe():
    redis = FakeAsyncRedis()
    candidate = _candidate(policy=_policy())
    executor = FakeExecutor()
    sweeper = IdlePauseSweeper(
        namespace_resolver=lambda: ["ns"],
        inventory=FakeInventory([candidate]),
        activity=NoClearRegistry(redis, slip_at_read=False),  # type: ignore[arg-type]
        locks=PauseLockManager(redis, lock_ttl_seconds=30),  # type: ignore[arg-type]
        executor=executor,  # type: ignore[arg-type]
        sweep_interval_seconds=15,
        clock=lambda: NOW_MS,
    )
    registry = ActivityRegistry(redis, activity_ttl_seconds=1800)  # type: ignore[arg-type]
    await redis.set(unknown_tombstone_key(candidate.sandbox_id), NOW_MS - (X + 10) * 1000, ex=3600)
    await registry.record(candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000)

    stats = await sweeper.sweep_once()  # must not raise

    assert len(executor.calls) == 1  # value-based decision unaffected
    assert stats.paused == 1


def _ingress(route_mode: str) -> IngressConfig:
    address = "*.gw.example.com" if route_mode == "wildcard" else "gw.example.com"
    return IngressConfig(
        mode="gateway",
        gateway=GatewayConfig(
            address=address,
            route=GatewayRouteModeConfig(mode=route_mode),
        ),
        secure_access=SecureAccessConfig(
            active_key="a",
            keys=[SecureAccessKey(key_id="a", key="c2VjcmV0")],
        ),
    )


@pytest.mark.parametrize(
    ("route_mode", "expected"),
    [
        ("header", True),
        ("uri", True),
        # wildcard is a legal gateway mode but build_endpoint cannot route
        # probes there; the prober must stay unbuilt so declared-probe
        # candidates keep the pause-unavailable exemption.
        ("wildcard", False),
    ],
)
def test_prober_build_follows_gateway_route_mode(route_mode, expected):
    app_config = SimpleNamespace(ingress=_ingress(route_mode))
    prober = _build_activity_prober(app_config)  # type: ignore[arg-type]
    assert (prober is not None) is expected


@pytest.mark.asyncio
async def test_stop_leaves_injected_prober_open():
    class _OwnedTrackingProber(_StubProber):
        def __init__(self):
            super().__init__([(PROBE_OUTCOME_NOT_ACTIVE, None)])
            self.closed = False

        async def aclose(self) -> None:
            self.closed = True

    redis = FakeAsyncRedis()
    candidate = _probe_candidate()
    executor = FakeExecutor()
    await ActivityRegistry(redis, activity_ttl_seconds=1800).record(  # type: ignore[arg-type]
        candidate.sandbox_id, now_ms=NOW_MS - (X + 10) * 1000
    )
    prober = _OwnedTrackingProber()
    sweeper = _sweeper([candidate], redis, executor, prober=prober)

    await sweeper.sweep_once()
    await sweeper.stop()

    assert len(executor.calls) == 1
    assert prober.closed is False  # injected probers stay their caller's
