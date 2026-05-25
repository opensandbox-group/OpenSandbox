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
