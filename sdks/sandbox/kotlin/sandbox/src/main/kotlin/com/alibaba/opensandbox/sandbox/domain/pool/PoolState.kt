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

/**
 * High-level state of the sandbox pool.
 *
 * Transitions:
 * - HEALTHY -> DEGRADED: consecutive create failures reach threshold
 * - DEGRADED -> HEALTHY: probe or create succeeds, failure counter resets
 * - HEALTHY/DEGRADED -> DRAINING: shutdown(graceful=true) called; pool replenish stops
 * - any -> STOPPED: shutdown(graceful=false) or drain completes
 */
enum class PoolState {
    /** Pool is operating normally. */
    HEALTHY,

    /** Replenish is failing; backoff applied; acquire still served from existing idle. */
    DEGRADED,

    /** Graceful shutdown in progress; no new replenish, waiting for in-flight ops. */
    DRAINING,

    /** Pool is stopped; no replenish and acquire() is rejected. */
    STOPPED,
}
