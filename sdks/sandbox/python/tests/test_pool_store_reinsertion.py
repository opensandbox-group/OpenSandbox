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

from datetime import timedelta

import pytest

from opensandbox.pool import InMemoryAsyncPoolStateStore, InMemoryPoolStateStore


@pytest.mark.parametrize("min_remaining_ttl", [None, timedelta(seconds=60)])
def test_sync_remove_reinsert_preserves_unique_fifo_membership(
    min_remaining_ttl: timedelta | None,
) -> None:
    store = InMemoryPoolStateStore()
    store.remove_idle("pool", "absent")
    store.put_idle("pool", "A")
    store.put_idle("pool", "B")
    store.put_idle("other-pool", "A")

    for _ in range(3):
        store.remove_idle("pool", "A")
        store.remove_idle("pool", "A")
        store.remove_idle("pool", "absent")
        store.put_idle("pool", "A")
        store.put_idle("pool", "A")
        entries = store.snapshot_idle_entries("pool")
        assert [entry.sandbox_id for entry in entries] == ["B", "A"]
        assert store.snapshot_counters("pool").idle_count == len(entries) == 2
        assert [
            entry.sandbox_id for entry in store.snapshot_idle_entries("other-pool")
        ] == ["A"]

    taken = []
    for _ in range(3):
        if min_remaining_ttl is None:
            taken.append(store.try_take_idle("pool"))
        else:
            taken.append(
                store.try_take_idle_min_ttl("pool", min_remaining_ttl).sandbox_id
            )
    assert taken == ["B", "A", None]
    assert store.snapshot_idle_entries("pool") == []
    assert store.try_take_idle("other-pool") == "A"


@pytest.mark.asyncio
@pytest.mark.parametrize("min_remaining_ttl", [None, timedelta(seconds=60)])
async def test_async_remove_reinsert_preserves_unique_fifo_membership(
    min_remaining_ttl: timedelta | None,
) -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.remove_idle("pool", "absent")
    await store.put_idle("pool", "A")
    await store.put_idle("pool", "B")
    await store.put_idle("other-pool", "A")

    for _ in range(3):
        await store.remove_idle("pool", "A")
        await store.remove_idle("pool", "A")
        await store.remove_idle("pool", "absent")
        await store.put_idle("pool", "A")
        await store.put_idle("pool", "A")
        entries = await store.snapshot_idle_entries("pool")
        assert [entry.sandbox_id for entry in entries] == ["B", "A"]
        assert (await store.snapshot_counters("pool")).idle_count == len(entries) == 2
        assert [
            entry.sandbox_id
            for entry in await store.snapshot_idle_entries("other-pool")
        ] == ["A"]

    taken = []
    for _ in range(3):
        if min_remaining_ttl is None:
            taken.append(await store.try_take_idle("pool"))
        else:
            result = await store.try_take_idle_min_ttl("pool", min_remaining_ttl)
            taken.append(result.sandbox_id)
    assert taken == ["B", "A", None]
    assert await store.snapshot_idle_entries("pool") == []
    assert await store.try_take_idle("other-pool") == "A"
