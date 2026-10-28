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

"""Per-sandbox pause admission lock (``SET NX EX``).

Admits one replica per idle episode. Correctness never depends on it —
FastPath PauseSandbox is an idempotent CAS — so a crashed replica's lock just
expires by TTL; release is a best-effort window shortener.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from opensandbox_server.integrations.idle_pause.constants import LOCK_PREFIX

if TYPE_CHECKING:
    from redis.asyncio import Redis

# Delete KEYS[1] only when its value still equals the owner (ARGV[1]).
_RELEASE_SCRIPT = """
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
"""


def lock_key(sandbox_id: str) -> str:
    return f"{LOCK_PREFIX}:{sandbox_id}"


class PauseLockManager:
    def __init__(self, client: "Redis", *, lock_ttl_seconds: int = 30):
        self._client = client
        self._ttl = int(lock_ttl_seconds)
        self._release_script = self._client.register_script(_RELEASE_SCRIPT)

    async def try_acquire(self, sandbox_id: str, owner: str) -> bool:
        """Acquire or report contention (False = another replica is handling it)."""
        acquired = await self._client.set(
            lock_key(sandbox_id),
            owner,
            nx=True,
            ex=self._ttl,
        )
        return bool(acquired)

    async def release(self, sandbox_id: str, owner: str) -> None:
        """Best-effort early release; never raises on a lost/expired lock."""
        await self._release_script(keys=[lock_key(sandbox_id)], args=[owner])
