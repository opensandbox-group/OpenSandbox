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
 * Result of a near-expiry-aware [PoolStateStore.tryTakeIdle].
 *
 * @property sandboxId The chosen idle sandbox ID, or null if no entry satisfied the threshold.
 * @property discardedAliveSandboxIds IDs that were skipped because their remaining TTL was
 * below the configured `minRemainingTtl`. These sandboxes are still **alive on the server**
 * (their server-side TTL has not elapsed yet) — callers should best-effort terminate them
 * so they do not linger past their pool membership and consume quota until expiry.
 *
 * Already-expired entries (server-side TTL has elapsed) are intentionally not included:
 * the server has already reaped them and a kill call would be a wasted round-trip.
 */
data class TakeIdleResult(
    val sandboxId: String?,
    val discardedAliveSandboxIds: List<String> = emptyList(),
) {
    companion object {
        @JvmStatic
        val EMPTY: TakeIdleResult = TakeIdleResult(sandboxId = null)

        @JvmStatic
        fun of(sandboxId: String?): TakeIdleResult = if (sandboxId == null) EMPTY else TakeIdleResult(sandboxId)
    }
}
