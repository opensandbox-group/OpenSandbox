# Copyright 2026 Alibaba Group Holding Ltd.
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

import asyncio
import threading
import time

import pytest

from opensandbox.adapters.endpoint_cache import AsyncEndpointCache, EndpointCache
from opensandbox.models.sandboxes import SandboxEndpoint


def _ep(addr: str) -> SandboxEndpoint:
    return SandboxEndpoint(endpoint=addr, headers={})


class TestEndpointCacheSync:
    def test_get_put(self):
        c = EndpointCache(maxsize=10, ttl=60.0)
        key = ("sb-1", 8080, False)
        assert c.get(key) is None
        c.put(key, _ep("localhost:8080"))
        assert c.get(key) is not None
        assert c.get(key).endpoint == "localhost:8080"

    def test_ttl_expiry(self):
        c = EndpointCache(maxsize=10, ttl=0.05)
        key = ("sb-1", 8080, False)
        c.put(key, _ep("localhost:8080"))
        assert c.get(key) is not None
        time.sleep(0.06)
        assert c.get(key) is None

    def test_lru_eviction(self):
        c = EndpointCache(maxsize=3, ttl=60.0)
        for i in range(3):
            c.put((f"sb-{i}", 8080, False), _ep(f"host-{i}:8080"))

        # Access sb-0 to make it recently used
        c.get(("sb-0", 8080, False))
        # Insert 4th, should evict sb-1
        c.put(("sb-3", 8080, False), _ep("host-3:8080"))

        assert c.get(("sb-1", 8080, False)) is None
        assert c.get(("sb-0", 8080, False)) is not None

    def test_invalidate(self):
        c = EndpointCache(maxsize=10, ttl=60.0)
        c.put(("sb-1", 8080, False), _ep("a"))
        c.put(("sb-1", 18080, False), _ep("b"))
        c.put(("sb-2", 8080, False), _ep("c"))
        c.invalidate("sb-1")
        assert c.get(("sb-1", 8080, False)) is None
        assert c.get(("sb-1", 18080, False)) is None
        assert c.get(("sb-2", 8080, False)) is not None

    def test_get_or_fetch_dedup(self):
        c = EndpointCache(maxsize=10, ttl=60.0)
        key = ("sb-1", 8080, False)
        fetch_count = [0]

        def fetch():
            fetch_count[0] += 1
            time.sleep(0.05)
            return _ep("result")

        threads = []
        results = []

        def worker():
            results.append(c.get_or_fetch(key, fetch))

        for _ in range(5):
            t = threading.Thread(target=worker)
            threads.append(t)
            t.start()
        for t in threads:
            t.join()

        assert fetch_count[0] == 1
        assert all(r.endpoint == "result" for r in results)

    def test_get_or_fetch_cache_hit(self):
        c = EndpointCache(maxsize=10, ttl=60.0)
        key = ("sb-1", 8080, False)
        c.put(key, _ep("cached"))
        fetch_count = [0]

        def fetch():
            fetch_count[0] += 1
            return _ep("fetched")

        result = c.get_or_fetch(key, fetch)
        assert result.endpoint == "cached"
        assert fetch_count[0] == 0


class TestAsyncEndpointCache:
    @pytest.mark.asyncio
    async def test_get_put(self):
        c = AsyncEndpointCache(maxsize=10, ttl=60.0)
        key = ("sb-1", 8080, False)
        assert c.get(key) is None
        c.put(key, _ep("localhost:8080"))
        assert c.get(key).endpoint == "localhost:8080"

    @pytest.mark.asyncio
    async def test_ttl_expiry(self):
        c = AsyncEndpointCache(maxsize=10, ttl=0.05)
        key = ("sb-1", 8080, False)
        c.put(key, _ep("localhost:8080"))
        assert c.get(key) is not None
        await asyncio.sleep(0.06)
        assert c.get(key) is None

    @pytest.mark.asyncio
    async def test_lru_eviction(self):
        c = AsyncEndpointCache(maxsize=3, ttl=60.0)
        for i in range(3):
            c.put((f"sb-{i}", 8080, False), _ep(f"host-{i}:8080"))
        c.get(("sb-0", 8080, False))
        c.put(("sb-3", 8080, False), _ep("host-3:8080"))
        assert c.get(("sb-1", 8080, False)) is None
        assert c.get(("sb-0", 8080, False)) is not None

    @pytest.mark.asyncio
    async def test_invalidate(self):
        c = AsyncEndpointCache(maxsize=10, ttl=60.0)
        c.put(("sb-1", 8080, False), _ep("a"))
        c.put(("sb-1", 18080, False), _ep("b"))
        c.put(("sb-2", 8080, False), _ep("c"))
        c.invalidate("sb-1")
        assert c.get(("sb-1", 8080, False)) is None
        assert c.get(("sb-2", 8080, False)) is not None

    @pytest.mark.asyncio
    async def test_get_or_fetch_dedup(self):
        c = AsyncEndpointCache(maxsize=10, ttl=60.0)
        key = ("sb-1", 8080, False)
        fetch_count = [0]

        async def fetch():
            fetch_count[0] += 1
            await asyncio.sleep(0.05)
            return _ep("result")

        results = await asyncio.gather(*[c.get_or_fetch(key, fetch) for _ in range(5)])
        assert fetch_count[0] == 1
        assert all(r.endpoint == "result" for r in results)

    @pytest.mark.asyncio
    async def test_get_or_fetch_error(self):
        c = AsyncEndpointCache(maxsize=10, ttl=60.0)
        key = ("sb-1", 8080, False)

        async def fetch():
            raise RuntimeError("network error")

        with pytest.raises(RuntimeError, match="network error"):
            await c.get_or_fetch(key, fetch)

        # Cache should not be populated on error
        assert c.get(key) is None
