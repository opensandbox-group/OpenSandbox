# Copyright 2025 The OpenSandbox Authors
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

import time
import threading
from unittest.mock import patch

import pytest

from opensandbox_server.services.k8s.rate_limiter import TokenBucketRateLimiter

class TestTokenBucketRateLimiter:

    def test_invalid_qps_raises_value_error(self):
        with pytest.raises(ValueError, match="qps must be > 0"):
            TokenBucketRateLimiter(qps=0)

    def test_negative_qps_raises_value_error(self):
        with pytest.raises(ValueError):
            TokenBucketRateLimiter(qps=-1.0)

    def test_burst_defaults_to_qps_when_zero(self):
        limiter = TokenBucketRateLimiter(qps=5.0, burst=0)
        assert limiter._burst == 5.0

    def test_explicit_burst_is_respected(self):
        limiter = TokenBucketRateLimiter(qps=5.0, burst=20)
        assert limiter._burst == 20.0

    def test_burst_minimum_is_one_when_qps_below_one(self):
        limiter = TokenBucketRateLimiter(qps=0.5)
        assert limiter._burst == 1.0

    def test_low_qps_limiter_can_acquire(self):
        limiter = TokenBucketRateLimiter(qps=0.5)
        assert limiter.try_acquire() is True

    def test_try_acquire_succeeds_when_bucket_full(self):
        limiter = TokenBucketRateLimiter(qps=10.0, burst=10)
        assert limiter.try_acquire() is True

    def test_try_acquire_fails_when_bucket_empty(self):
        limiter = TokenBucketRateLimiter(qps=1.0, burst=1)
        limiter.try_acquire()
        assert limiter.try_acquire() is False

    def test_try_acquire_consumes_token(self):
        limiter = TokenBucketRateLimiter(qps=10.0, burst=3)
        assert limiter.try_acquire() is True
        assert limiter.try_acquire() is True
        assert limiter.try_acquire() is True
        assert limiter.try_acquire() is False

    def test_acquire_succeeds_immediately_when_tokens_available(self):
        limiter = TokenBucketRateLimiter(qps=100.0, burst=10)
        start = time.monotonic()
        limiter.acquire()
        elapsed = time.monotonic() - start
        assert elapsed < 0.1

    def test_acquire_blocks_until_token_available(self):
        limiter = TokenBucketRateLimiter(qps=10.0, burst=1)
        limiter.try_acquire()

        start = time.monotonic()
        limiter.acquire()  # should wait ~0.1s for next token
        elapsed = time.monotonic() - start

        assert elapsed >= 0.05

    def test_acquire_minimum_sleep_prevents_busy_loop(self):
        limiter = TokenBucketRateLimiter(qps=1.0, burst=1)
        # Manually set tokens to just below 1 to produce a near-zero wait
        with limiter._lock:
            limiter._tokens = 1.0 - 1e-10

        with patch("opensandbox_server.services.k8s.rate_limiter.time.sleep") as mock_sleep:
            # _try_acquire will succeed on first or second call; we only care
            # that if sleep is called, the argument is >= 0.001.
            limiter.acquire()
            for call in mock_sleep.call_args_list:
                assert call.args[0] >= 0.001

    def test_tokens_refill_over_time(self):
        limiter = TokenBucketRateLimiter(qps=100.0, burst=10)
        for _ in range(10):
            limiter.try_acquire()
        assert limiter.try_acquire() is False

        time.sleep(0.05)  # wait for ~5 tokens to refill at 100 qps

        assert limiter.try_acquire() is True

    def test_tokens_capped_at_burst(self):
        limiter = TokenBucketRateLimiter(qps=10.0, burst=5)
        # Simulate a long elapsed gap in a single refill: tokens must stay capped at burst.
        future = limiter._last_refill + 3600
        with patch("opensandbox_server.services.k8s.rate_limiter.time.monotonic", return_value=future):
            with limiter._lock:
                limiter._refill()
        assert limiter._tokens <= 5.0

    def test_concurrent_acquires_do_not_exceed_burst(self):
        burst = 5
        limiter = TokenBucketRateLimiter(qps=1000.0, burst=burst)
        successes = []
        lock = threading.Lock()

        # Freeze time so _refill() never adds extra tokens during the test
        fixed_time = limiter._last_refill

        def worker():
            with patch("opensandbox_server.services.k8s.rate_limiter.time.monotonic", return_value=fixed_time):
                if limiter.try_acquire():
                    with lock:
                        successes.append(1)

        threads = [threading.Thread(target=worker) for _ in range(20)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()

        assert len(successes) <= burst
