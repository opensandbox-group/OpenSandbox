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
 * Policy for acquire when the idle buffer is empty.
 *
 * - FAIL_FAST: throw [PoolEmptyException] (POOL_EMPTY) when idle is empty,
 *   or [PoolAcquireFailedException] (POOL_ACQUIRE_FAILED) when idle candidate is unusable.
 * - DIRECT_CREATE: attempt direct create via lifecycle API, then connect and return.
 */
enum class AcquirePolicy {
    /** When no idle sandbox is available, fail immediately with POOL_EMPTY. */
    FAIL_FAST,

    /** When no idle sandbox is available, create a new sandbox via lifecycle API. */
    DIRECT_CREATE,
}
