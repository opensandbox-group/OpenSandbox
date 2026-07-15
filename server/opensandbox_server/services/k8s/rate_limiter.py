# Copyright 2025 Alibaba Group Holding Ltd.
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

"""Generic token-bucket rate limiter."""

import threading
import time


class TokenBucketRateLimiter:
    """Thread-safe token-bucket rate limiter.

    Tokens refill at ``qps`` tokens per second up to a maximum of ``burst``.
    Calling :meth:`acquire` consumes one token, blocking if the bucket is empty.

    Args:
        qps: Sustained request rate in requests per second.
        burst: Maximum burst size (bucket capacity). Defaults to ``qps``,
               with a minimum of 1 to ensure at least one token is always
               available regardless of qps.
    """

    def __init__(self, qps: float, burst: float = 0.0) -> None:
        if qps <= 0:
            raise ValueError(f"qps must be > 0, got {qps}")
        self._qps = qps
        self._burst = max(burst if burst > 0 else qps, 1.0)
        self._tokens = self._burst
        self._last_refill = time.monotonic()
        self._lock = threading.Lock()

    def acquire(self) -> None:
        """Acquire one token, blocking until one is available."""
        while True:
            wait = self._try_acquire()
            if wait <= 0.0:
                return
            # Clamp to a minimum of 1 ms to avoid a busy-loop caused by
            # floating-point imprecision when the deficit is near-zero.
            time.sleep(max(wait, 0.001))

    def try_acquire(self) -> bool:
        """Try to acquire one token without blocking.

        Returns:
            ``True`` if a token was consumed, ``False`` if the bucket is empty.
        """
        return self._try_acquire() <= 0.0

    def _try_acquire(self) -> float:
        """Attempt to take a token.

        Returns:
            0.0 if a token was consumed successfully, otherwise the approximate
            number of seconds to wait before retrying.
        """
        with self._lock:
            self._refill()
            if self._tokens >= 1.0:
                self._tokens -= 1.0
                return 0.0
            return (1.0 - self._tokens) / self._qps

    def _refill(self) -> None:
        """Add tokens proportional to elapsed time (call with lock held)."""
        now = time.monotonic()
        elapsed = now - self._last_refill
        self._tokens = min(self._burst, self._tokens + elapsed * self._qps)
        self._last_refill = now
