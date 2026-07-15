from datetime import datetime, timedelta, timezone
from threading import Lock, Thread

from opensandbox.pool import InMemoryPoolStateStore


def test_in_memory_store_takes_idle_fifo_once() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "sandbox-1")
    store.put_idle("pool", "sandbox-2")

    assert store.try_take_idle("pool") == "sandbox-1"
    assert store.try_take_idle("pool") == "sandbox-2"
    assert store.try_take_idle("pool") is None
    assert store.snapshot_counters("pool").idle_count == 0


def test_in_memory_store_duplicate_put_has_single_membership() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "sandbox-1")
    store.put_idle("pool", "sandbox-1")

    assert store.snapshot_counters("pool").idle_count == 1
    assert store.try_take_idle("pool") == "sandbox-1"
    assert store.try_take_idle("pool") is None


def test_in_memory_store_reaps_expired_idle() -> None:
    store = InMemoryPoolStateStore()
    store.set_idle_entry_ttl("pool", timedelta(milliseconds=1))
    store.put_idle("pool", "sandbox-1")

    store.reap_expired_idle("pool", datetime.now(timezone.utc) + timedelta(seconds=1))

    assert store.try_take_idle("pool") is None
    assert store.snapshot_counters("pool").idle_count == 0


def test_in_memory_store_try_take_idle_min_ttl_surfaces_alive_below_threshold() -> None:
    store = InMemoryPoolStateStore()
    # Entries get a 5s TTL — still alive (server-side TTL has not elapsed) but below 60s.
    store.set_idle_entry_ttl("pool", timedelta(seconds=5))
    store.put_idle("pool", "sandbox-1")
    store.put_idle("pool", "sandbox-2")

    result = store.try_take_idle_min_ttl("pool", timedelta(seconds=60))
    assert result.sandbox_id is None
    assert set(result.discarded_alive_sandbox_ids) == {"sandbox-1", "sandbox-2"}
    assert store.snapshot_counters("pool").idle_count == 0


def test_in_memory_store_try_take_idle_min_ttl_silently_drops_expired() -> None:
    """Already-expired entries are dropped without surfacing — the server has reaped them."""
    import time

    store = InMemoryPoolStateStore()
    store.set_idle_entry_ttl("pool", timedelta(milliseconds=1))
    store.put_idle("pool", "expired")
    time.sleep(0.02)
    store.set_idle_entry_ttl("pool", timedelta(minutes=10))
    store.put_idle("pool", "alive")

    result = store.try_take_idle_min_ttl("pool", timedelta(seconds=60))
    assert result.sandbox_id == "alive"
    assert result.discarded_alive_sandbox_ids == ()


def test_in_memory_store_try_take_idle_min_ttl_returns_entries_above_threshold() -> None:
    store = InMemoryPoolStateStore()
    store.set_idle_entry_ttl("pool", timedelta(minutes=10))
    store.put_idle("pool", "sandbox-1")

    result = store.try_take_idle_min_ttl("pool", timedelta(seconds=60))
    assert result.sandbox_id == "sandbox-1"
    assert result.discarded_alive_sandbox_ids == ()


def test_in_memory_store_try_take_idle_min_ttl_zero_falls_back_to_base() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "sandbox-1")

    first = store.try_take_idle_min_ttl("pool", timedelta(0))
    assert first.sandbox_id == "sandbox-1"
    assert first.discarded_alive_sandbox_ids == ()

    store.put_idle("pool", "sandbox-2")
    second = store.try_take_idle_min_ttl("pool", timedelta(seconds=-1))
    assert second.sandbox_id == "sandbox-2"
    assert second.discarded_alive_sandbox_ids == ()


def test_in_memory_store_reap_expired_idle_min_ttl_returns_alive_evicted() -> None:
    store = InMemoryPoolStateStore()
    store.set_idle_entry_ttl("pool", timedelta(seconds=5))
    store.put_idle("pool", "sandbox-1")
    store.put_idle("pool", "sandbox-2")

    discarded_alive = store.reap_expired_idle_min_ttl(
        "pool", datetime.now(timezone.utc), timedelta(seconds=60)
    )

    assert set(discarded_alive) == {"sandbox-1", "sandbox-2"}
    assert store.snapshot_counters("pool").idle_count == 0


def test_in_memory_store_reap_expired_idle_min_ttl_keeps_above_threshold() -> None:
    store = InMemoryPoolStateStore()
    store.set_idle_entry_ttl("pool", timedelta(minutes=10))
    store.put_idle("pool", "sandbox-1")

    discarded_alive = store.reap_expired_idle_min_ttl(
        "pool", datetime.now(timezone.utc), timedelta(seconds=60)
    )

    assert discarded_alive == ()
    assert store.snapshot_counters("pool").idle_count == 1


def test_in_memory_store_reap_expired_idle_min_ttl_excludes_already_expired() -> None:
    """Already-expired entries are evicted but not returned — server has reaped them."""
    import time

    store = InMemoryPoolStateStore()
    store.set_idle_entry_ttl("pool", timedelta(milliseconds=1))
    store.put_idle("pool", "expired")
    time.sleep(0.02)
    store.set_idle_entry_ttl("pool", timedelta(seconds=5))
    store.put_idle("pool", "alive")

    discarded_alive = store.reap_expired_idle_min_ttl(
        "pool", datetime.now(timezone.utc), timedelta(seconds=60)
    )

    assert discarded_alive == ("alive",)
    assert store.snapshot_counters("pool").idle_count == 0


def test_in_memory_store_concurrent_take_is_unique() -> None:
    store = InMemoryPoolStateStore()
    for i in range(100):
        store.put_idle("pool", f"sandbox-{i}")

    taken: set[str] = set()
    errors: list[Exception] = []
    taken_lock = Lock()

    def worker() -> None:
        try:
            while True:
                sandbox_id = store.try_take_idle("pool")
                if sandbox_id is None:
                    return
                with taken_lock:
                    if sandbox_id in taken:
                        raise AssertionError(f"duplicate take: {sandbox_id}")
                    taken.add(sandbox_id)
        except Exception as exc:
            errors.append(exc)

    threads = [Thread(target=worker) for _ in range(8)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()

    assert errors == []
    assert len(taken) == 100
    assert store.snapshot_counters("pool").idle_count == 0
