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

package com.alibaba.opensandbox.sandbox.infrastructure.pool

import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolStateStoreUnavailableException
import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.domain.pool.StoreCounters
import redis.clients.jedis.UnifiedJedis
import redis.clients.jedis.params.SetParams
import java.time.Duration
import java.time.Instant
import java.util.Base64

/**
 * Redis-backed [PoolStateStore] for coordinating a sandbox pool across processes.
 *
 * State is namespaced by pool name:
 * - idle list: FIFO sandbox IDs
 * - idle expires hash: sandboxId -> expiresAt epoch millis
 * - primary lock: ownerId with Redis key TTL
 * - max idle: cluster-wide target written by [com.alibaba.opensandbox.sandbox.pool.SandboxPool.resize]
 *
 * Compound operations use Lua scripts so take, put, and owner-checked lock updates are atomic.
 *
 * This store intentionally does not create, configure, or close the Redis client. Callers own
 * the lifecycle and may pass any Jedis [UnifiedJedis] implementation appropriate for their environment.
 * The provided client must be safe for concurrent use because pool acquire, reconcile, resize,
 * snapshot, and release operations may call the store from different threads. [redis.clients.jedis.JedisPooled]
 * is the recommended client type. Do not share a single non-pooled Jedis connection across pool threads.
 */
class RedisPoolStateStore(
    private val redis: UnifiedJedis,
    private val keyPrefix: String = DEFAULT_KEY_PREFIX,
) : PoolStateStore {
    private val defaultIdleTtl: Duration = Duration.ofHours(24)

    override fun tryTakeIdle(poolName: String): String? =
        execute("tryTakeIdle", poolName) {
            val result =
                redis.eval(
                    TAKE_IDLE_SCRIPT,
                    listOf(idleListKey(poolName), idleExpiresKey(poolName)),
                    emptyList<String>(),
                )
            result as? String
        }

    override fun putIdle(
        poolName: String,
        sandboxId: String,
    ) {
        require(sandboxId.isNotBlank()) { "sandboxId must not be blank" }
        execute("putIdle", poolName) {
            val idleTtlMillis = resolveIdleTtl(poolName).toMillis().coerceAtLeast(1).toString()
            redis.eval(
                PUT_IDLE_SCRIPT,
                listOf(idleListKey(poolName), idleExpiresKey(poolName)),
                listOf(sandboxId, idleTtlMillis),
            )
        }
    }

    override fun removeIdle(
        poolName: String,
        sandboxId: String,
    ) {
        execute("removeIdle", poolName) {
            redis.hdel(idleExpiresKey(poolName), sandboxId)
            redis.lrem(idleListKey(poolName), 0, sandboxId)
        }
    }

    override fun tryAcquirePrimaryLock(
        poolName: String,
        ownerId: String,
        ttl: Duration,
    ): Boolean {
        validateOwnerAndTtl(ownerId, ttl)
        return execute("tryAcquirePrimaryLock", poolName) {
            val ttlMillis = ttl.toMillis().coerceAtLeast(1)
            val acquired = redis.set(primaryLockKey(poolName), ownerId, SetParams.setParams().nx().px(ttlMillis))
            if (acquired == "OK") {
                true
            } else {
                renewPrimaryLock(poolName, ownerId, ttl)
            }
        }
    }

    override fun renewPrimaryLock(
        poolName: String,
        ownerId: String,
        ttl: Duration,
    ): Boolean {
        validateOwnerAndTtl(ownerId, ttl)
        return execute("renewPrimaryLock", poolName) {
            val result =
                redis.eval(
                    RENEW_LOCK_SCRIPT,
                    listOf(primaryLockKey(poolName)),
                    listOf(ownerId, ttl.toMillis().coerceAtLeast(1).toString()),
                )
            result == 1L
        }
    }

    override fun releasePrimaryLock(
        poolName: String,
        ownerId: String,
    ) {
        execute("releasePrimaryLock", poolName) {
            redis.eval(RELEASE_LOCK_SCRIPT, listOf(primaryLockKey(poolName)), listOf(ownerId))
        }
    }

    override fun reapExpiredIdle(
        poolName: String,
        now: Instant,
    ) {
        execute("reapExpiredIdle", poolName) {
            redis.eval(
                REAP_EXPIRED_SCRIPT,
                listOf(idleListKey(poolName), idleExpiresKey(poolName)),
                emptyList<String>(),
            )
        }
    }

    override fun snapshotCounters(poolName: String): StoreCounters =
        execute("snapshotCounters", poolName) {
            StoreCounters(redis.hlen(idleExpiresKey(poolName)).toInt())
        }

    override fun snapshotIdleEntries(poolName: String): List<IdleEntry> =
        execute("snapshotIdleEntries", poolName) {
            val ids = redis.lrange(idleListKey(poolName), 0, -1)
            val expiresById = redis.hgetAll(idleExpiresKey(poolName))
            ids.mapNotNull { sandboxId ->
                val expiresAtMillis = expiresById[sandboxId]?.toLongOrNull() ?: return@mapNotNull null
                IdleEntry(sandboxId, Instant.ofEpochMilli(expiresAtMillis))
            }
        }

    override fun getMaxIdle(poolName: String): Int? =
        execute("getMaxIdle", poolName) {
            redis.get(maxIdleKey(poolName))?.toIntOrNull()
        }

    override fun setMaxIdle(
        poolName: String,
        maxIdle: Int,
    ) {
        require(maxIdle >= 0) { "maxIdle must be >= 0" }
        execute("setMaxIdle", poolName) {
            redis.set(maxIdleKey(poolName), maxIdle.toString())
        }
    }

    override fun setIdleEntryTtl(
        poolName: String,
        idleTtl: Duration,
    ) {
        require(!idleTtl.isNegative && !idleTtl.isZero) { "idleTtl must be positive" }
        execute("setIdleEntryTtl", poolName) {
            redis.set(idleTtlKey(poolName), idleTtl.toMillis().coerceAtLeast(1).toString())
        }
    }

    private fun resolveIdleTtl(poolName: String): Duration {
        val ttlMillis =
            execute("resolveIdleTtl", poolName) {
                redis.get(idleTtlKey(poolName))?.toLongOrNull()
            } ?: return defaultIdleTtl
        return Duration.ofMillis(ttlMillis.coerceAtLeast(1))
    }

    private fun validateOwnerAndTtl(
        ownerId: String,
        ttl: Duration,
    ) {
        require(ownerId.isNotBlank()) { "ownerId must not be blank" }
        require(!ttl.isNegative && !ttl.isZero) { "ttl must be positive" }
    }

    private fun idleListKey(poolName: String): String = poolKey(poolName, "idle:list")

    private fun idleExpiresKey(poolName: String): String = poolKey(poolName, "idle:expires")

    private fun primaryLockKey(poolName: String): String = poolKey(poolName, "lock")

    private fun maxIdleKey(poolName: String): String = poolKey(poolName, "maxIdle")

    private fun idleTtlKey(poolName: String): String = poolKey(poolName, "idleTtlMillis")

    private fun poolKey(
        poolName: String,
        suffix: String,
    ): String {
        require(poolName.isNotBlank()) { "poolName must not be blank" }
        val hashTag = Base64.getUrlEncoder().withoutPadding().encodeToString(poolName.toByteArray(Charsets.UTF_8))
        return "$keyPrefix:{$hashTag}:$suffix"
    }

    private fun <T> execute(
        operation: String,
        poolName: String,
        block: () -> T,
    ): T {
        return try {
            block()
        } catch (e: IllegalArgumentException) {
            throw e
        } catch (e: Exception) {
            throw PoolStateStoreUnavailableException(
                message = "Redis pool state store operation failed: operation=$operation poolName=$poolName",
                cause = e,
            )
        }
    }

    companion object {
        const val DEFAULT_KEY_PREFIX = "opensandbox:pool"

        private const val TAKE_IDLE_SCRIPT =
            """
            local redis_time = redis.call('TIME')
            local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
            while true do
              local sandbox_id = redis.call('LPOP', KEYS[1])
              if not sandbox_id then
                return nil
              end
              local expires_at = redis.call('HGET', KEYS[2], sandbox_id)
              if expires_at then
                redis.call('HDEL', KEYS[2], sandbox_id)
                if tonumber(expires_at) > now_ms then
                  return sandbox_id
                end
              end
            end
            """

        private const val PUT_IDLE_SCRIPT =
            """
            local redis_time = redis.call('TIME')
            local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
            local expires_at = now_ms + tonumber(ARGV[2])
            local current_expires_at = redis.call('HGET', KEYS[2], ARGV[1])
            if current_expires_at and tonumber(current_expires_at) > now_ms then
              return 0
            end
            if not current_expires_at then
              redis.call('RPUSH', KEYS[1], ARGV[1])
            end
            redis.call('HSET', KEYS[2], ARGV[1], expires_at)
            return 1
            """

        private const val RENEW_LOCK_SCRIPT =
            """
            if redis.call('GET', KEYS[1]) == ARGV[1] then
              redis.call('PEXPIRE', KEYS[1], ARGV[2])
              return 1
            end
            return 0
            """

        private const val RELEASE_LOCK_SCRIPT =
            """
            if redis.call('GET', KEYS[1]) == ARGV[1] then
              return redis.call('DEL', KEYS[1])
            end
            return 0
            """

        private const val REAP_EXPIRED_SCRIPT =
            """
            local redis_time = redis.call('TIME')
            local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
            local entries = redis.call('HGETALL', KEYS[2])
            for i = 1, #entries, 2 do
              if tonumber(entries[i + 1]) <= now_ms then
                redis.call('HDEL', KEYS[2], entries[i])
                redis.call('LREM', KEYS[1], 0, entries[i])
              end
            end
            return 1
            """
    }
}
