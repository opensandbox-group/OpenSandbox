/*
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.sandbox.domain.pool

import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolAcquireFailedException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolEmptyException

/**
 * Policy for acquire when the idle buffer is empty or an idle candidate fails its ready-check.
 *
 * - [FAIL_FAST]: throw [PoolEmptyException] (`POOL_EMPTY`) when idle is empty,
 *   or [PoolAcquireFailedException] (`POOL_ACQUIRE_FAILED`) when the first idle candidate is unusable.
 *   No retry across multiple idle candidates.
 * - [DIRECT_CREATE]: on empty idle or first-candidate failure, attempt direct create via lifecycle API.
 *   No retry across multiple idle candidates.
 * - [RETRY_NEXT_IDLE]: on first-candidate failure, skip the bad candidate and try the next idle up to
 *   [PoolConfig.maxAcquireRetries] total idle attempts. If all attempts fail (or idle became empty
 *   during the loop), throw [PoolAcquireFailedException] / [PoolEmptyException] like [FAIL_FAST].
 * - [RETRY_NEXT_IDLE_THEN_CREATE]: same as [RETRY_NEXT_IDLE], but on exhaustion fall through to
 *   direct create instead of throwing. Useful when the pool contains a mix of healthy and stale
 *   idle sandboxes (e.g. after a partial network flap or a slow-cold-starting custom template)
 *   and callers want to prefer a warm sandbox but never block on repeated cold starts.
 */
enum class AcquirePolicy {
    /** When no idle sandbox is available, fail immediately with `POOL_EMPTY`. */
    FAIL_FAST,

    /** When no idle sandbox is available, create a new sandbox via lifecycle API. */
    DIRECT_CREATE,

    /**
     * Retry across up to [PoolConfig.maxAcquireRetries] idle candidates; if all fail, throw.
     * Equivalent to [FAIL_FAST] with a bounded skip-bad-idle loop.
     */
    RETRY_NEXT_IDLE,

    /**
     * Retry across up to [PoolConfig.maxAcquireRetries] idle candidates; if all fail, fall through
     * to direct create. Equivalent to [DIRECT_CREATE] with a bounded skip-bad-idle loop.
     */
    RETRY_NEXT_IDLE_THEN_CREATE,
}
