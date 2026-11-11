// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import {
  PoolDestroyedException,
  PoolStateStoreUnavailableException,
} from "./core/exceptions.js";
import {
  PoolDestroyState,
  type IdleEntry,
  type PoolStateStore,
  type ReapResult,
  type StoreCounters,
  type TakeIdleResult,
} from "./poolTypes.js";

/** Minimal surface implemented by node-redis and compatible Redis clients. */
export interface RedisPoolClient {
  sendCommand(command: string[]): Promise<unknown>;
}

export interface RedisPoolStateStoreOptions {
  client: RedisPoolClient;
  keyPrefix?: string;
}

const DEFAULT_IDLE_TTL_MILLIS = 24 * 60 * 60 * 1000;
const FENCED_WRITE_REJECTED = -1;

/** Redis-backed distributed pool state. The caller owns the Redis client lifecycle. */
export class RedisPoolStateStore implements PoolStateStore {
  static readonly DEFAULT_KEY_PREFIX = "opensandbox:pool";

  private readonly client: RedisPoolClient;
  private readonly keyPrefix: string;

  constructor(options: RedisPoolStateStoreOptions) {
    if (!options?.client) throw new Error("client is required");
    this.client = options.client;
    this.keyPrefix = options.keyPrefix ?? RedisPoolStateStore.DEFAULT_KEY_PREFIX;
  }

  async tryTakeIdle(poolName: string): Promise<string | undefined> {
    return (await this.tryTakeIdleWithMinTtl(poolName, 0)).sandboxId;
  }

  async tryTakeIdleWithMinTtl(
    poolName: string,
    minRemainingTtlSeconds: number,
  ): Promise<TakeIdleResult> {
    requireNonNegative(minRemainingTtlSeconds, "minRemainingTtlSeconds");
    return await this.execute("tryTakeIdle", poolName, async () => {
      const result = await this.eval(
        TAKE_IDLE_SCRIPT,
        [this.key(poolName, "idle:list"), this.key(poolName, "idle:expires")],
        [String(Math.max(0, Math.floor(minRemainingTtlSeconds * 1000)))],
      );
      return decodeTakeIdleResult(result);
    });
  }

  async putIdle(poolName: string, sandboxId: string): Promise<void> {
    if (!sandboxId.trim()) throw new Error("sandboxId must not be blank");
    await this.execute("putIdle", poolName, async () => {
      const ttlRaw = await this.client.sendCommand(["GET", this.key(poolName, "idleTtlMillis")]);
      const ttlMillis = parseInteger(ttlRaw) ?? DEFAULT_IDLE_TTL_MILLIS;
      await this.evalFencedWrite(
        "putIdle",
        poolName,
        PUT_IDLE_SCRIPT,
        [
          this.key(poolName, "idle:list"),
          this.key(poolName, "idle:expires"),
          this.key(poolName, "destroy:state"),
        ],
        [sandboxId, String(Math.max(1, ttlMillis))],
      );
    });
  }

  async removeIdle(poolName: string, sandboxId: string): Promise<void> {
    await this.execute("removeIdle", poolName, async () => {
      await this.client.sendCommand(["HDEL", this.key(poolName, "idle:expires"), sandboxId]);
      await this.client.sendCommand(["LREM", this.key(poolName, "idle:list"), "0", sandboxId]);
    });
  }

  async tryAcquirePrimaryLock(poolName: string, ownerId: string, ttlSeconds: number): Promise<boolean> {
    validateOwnerAndTtl(ownerId, ttlSeconds);
    return await this.execute("tryAcquirePrimaryLock", poolName, async () =>
      isOne(await this.eval(
        ACQUIRE_LOCK_SCRIPT,
        [this.key(poolName, "lock"), this.key(poolName, "destroy:state")],
        [ownerId, String(Math.max(1, Math.floor(ttlSeconds * 1000)))],
      )),
    );
  }

  async renewPrimaryLock(poolName: string, ownerId: string, ttlSeconds: number): Promise<boolean> {
    validateOwnerAndTtl(ownerId, ttlSeconds);
    return await this.execute("renewPrimaryLock", poolName, async () =>
      isOne(await this.eval(
        RENEW_LOCK_SCRIPT,
        [this.key(poolName, "lock"), this.key(poolName, "destroy:state")],
        [ownerId, String(Math.max(1, Math.floor(ttlSeconds * 1000)))],
      )),
    );
  }

  async releasePrimaryLock(poolName: string, ownerId: string): Promise<void> {
    await this.execute("releasePrimaryLock", poolName, async () => {
      await this.eval(RELEASE_LOCK_SCRIPT, [this.key(poolName, "lock")], [ownerId]);
    });
  }

  async reapExpiredIdle(poolName: string, _now: Date): Promise<void> {
    await this.reapExpiredIdleWithMinTtl(poolName, _now, 0);
  }

  async reapExpiredIdleWithMinTtl(
    poolName: string,
    _now: Date,
    minRemainingTtlSeconds: number,
  ): Promise<ReapResult> {
    requireNonNegative(minRemainingTtlSeconds, "minRemainingTtlSeconds");
    return await this.execute("reapExpiredIdle", poolName, async () => {
      const result = await this.eval(
        REAP_EXPIRED_SCRIPT,
        [this.key(poolName, "idle:list"), this.key(poolName, "idle:expires")],
        [String(Math.max(0, Math.floor(minRemainingTtlSeconds * 1000)))],
      );
      return { discardedAliveSandboxIds: asArray(result).map(asString) };
    });
  }

  async snapshotCounters(poolName: string): Promise<StoreCounters> {
    return await this.execute("snapshotCounters", poolName, async () => ({
      idleCount: parseInteger(await this.client.sendCommand(["HLEN", this.key(poolName, "idle:expires")])) ?? 0,
    }));
  }

  async snapshotIdleEntries(poolName: string): Promise<IdleEntry[]> {
    return await this.execute("snapshotIdleEntries", poolName, async () => {
      const ids = asArray(await this.client.sendCommand(["LRANGE", this.key(poolName, "idle:list"), "0", "-1"]))
        .map(asString);
      const rawExpires = asArray(await this.client.sendCommand(["HGETALL", this.key(poolName, "idle:expires")]));
      const expires = new Map<string, number>();
      for (let index = 0; index + 1 < rawExpires.length; index += 2) {
        const value = parseInteger(rawExpires[index + 1]);
        if (value !== undefined) expires.set(asString(rawExpires[index]), value);
      }
      return ids.flatMap((sandboxId) => {
        const expiresAt = expires.get(sandboxId);
        return expiresAt === undefined ? [] : [{ sandboxId, expiresAt: new Date(expiresAt) }];
      });
    });
  }

  async getMaxIdle(poolName: string): Promise<number | undefined> {
    return await this.execute("getMaxIdle", poolName, async () =>
      parseInteger(await this.client.sendCommand(["GET", this.key(poolName, "maxIdle")])),
    );
  }

  async setMaxIdle(poolName: string, maxIdle: number): Promise<void> {
    if (!Number.isInteger(maxIdle) || maxIdle < 0) throw new Error("maxIdle must be a non-negative integer");
    await this.execute("setMaxIdle", poolName, async () => {
      await this.evalFencedWrite(
        "setMaxIdle",
        poolName,
        SET_VALUE_SCRIPT,
        [this.key(poolName, "maxIdle"), this.key(poolName, "destroy:state")],
        [String(maxIdle)],
      );
    });
  }

  async setIdleEntryTtl(poolName: string, ttlSeconds: number): Promise<void> {
    requirePositive(ttlSeconds, "ttlSeconds");
    await this.execute("setIdleEntryTtl", poolName, async () => {
      await this.evalFencedWrite(
        "setIdleEntryTtl",
        poolName,
        SET_VALUE_SCRIPT,
        [this.key(poolName, "idleTtlMillis"), this.key(poolName, "destroy:state")],
        [String(Math.max(1, Math.floor(ttlSeconds * 1000)))],
      );
    });
  }

  async getDestroyState(poolName: string): Promise<PoolDestroyState> {
    return await this.execute("getDestroyState", poolName, async () => {
      const state = asOptionalString(await this.client.sendCommand(["GET", this.key(poolName, "destroy:state")]));
      if (state === PoolDestroyState.DESTROYING) return PoolDestroyState.DESTROYING;
      if (state === PoolDestroyState.DESTROYED) return PoolDestroyState.DESTROYED;
      return PoolDestroyState.ACTIVE;
    });
  }

  async beginDestroy(poolName: string, ownerId: string): Promise<void> {
    if (!ownerId.trim()) throw new Error("ownerId must not be blank");
    await this.execute("beginDestroy", poolName, async () => {
      const result = await this.eval(
        BEGIN_DESTROY_SCRIPT,
        [this.key(poolName, "destroy:state"), this.key(poolName, "destroy:owner")],
        [PoolDestroyState.DESTROYING, PoolDestroyState.DESTROYED, ownerId],
      );
      if (parseInteger(result) === FENCED_WRITE_REJECTED) {
        throw new PoolDestroyedException(poolName, PoolDestroyState.DESTROYED);
      }
    });
  }

  async clearPoolState(poolName: string): Promise<void> {
    await this.execute("clearPoolState", poolName, async () => {
      await this.client.sendCommand([
        "DEL",
        this.key(poolName, "idle:list"),
        this.key(poolName, "idle:expires"),
        this.key(poolName, "lock"),
        this.key(poolName, "maxIdle"),
        this.key(poolName, "idleTtlMillis"),
      ]);
    });
  }

  async markDestroyed(
    poolName: string,
    ownerId: string,
    tombstoneTtlSeconds?: number | null,
  ): Promise<void> {
    if (!ownerId.trim()) throw new Error("ownerId must not be blank");
    if (tombstoneTtlSeconds != null) requirePositive(tombstoneTtlSeconds, "tombstoneTtlSeconds");
    await this.execute("markDestroyed", poolName, async () => {
      const stateCommand = ["SET", this.key(poolName, "destroy:state"), PoolDestroyState.DESTROYED];
      const ownerCommand = ["SET", this.key(poolName, "destroy:owner"), ownerId];
      if (tombstoneTtlSeconds != null) {
        const ttlMillis = String(Math.max(1, Math.floor(tombstoneTtlSeconds * 1000)));
        stateCommand.push("PX", ttlMillis);
        ownerCommand.push("PX", ttlMillis);
      }
      await this.client.sendCommand(stateCommand);
      await this.client.sendCommand(ownerCommand);
    });
  }

  private async eval(script: string, keys: string[], args: string[]): Promise<unknown> {
    return await this.client.sendCommand(["EVAL", script, String(keys.length), ...keys, ...args]);
  }

  private async evalFencedWrite(
    operation: string,
    poolName: string,
    script: string,
    keys: string[],
    args: string[],
  ): Promise<void> {
    const result = await this.eval(script, keys, args);
    if (parseInteger(result) === FENCED_WRITE_REJECTED) {
      throw new PoolDestroyedException(poolName, await this.getDestroyState(poolName));
    }
    void operation;
  }

  private key(poolName: string, suffix: string): string {
    if (!poolName.trim()) throw new Error("poolName must not be blank");
    const bytes = new TextEncoder().encode(poolName);
    const encoded = btoa(String.fromCharCode(...bytes))
      .replaceAll("+", "-")
      .replaceAll("/", "_")
      .replace(/=+$/, "");
    return `${this.keyPrefix}:{${encoded}}:${suffix}`;
  }

  private async execute<T>(operation: string, poolName: string, call: () => Promise<T>): Promise<T> {
    try {
      return await call();
    } catch (cause) {
      if (cause instanceof PoolDestroyedException || cause instanceof PoolStateStoreUnavailableException) throw cause;
      throw new PoolStateStoreUnavailableException(`${operation} poolName=${poolName}`, cause);
    }
  }
}

function decodeTakeIdleResult(value: unknown): TakeIdleResult {
  if (value == null) return { discardedAliveSandboxIds: [] };
  if (!Array.isArray(value)) return { sandboxId: asString(value), discardedAliveSandboxIds: [] };
  const taken = value.length > 0 ? asString(value[0]) : "";
  const discarded = value.length > 1 ? asArray(value[1]).map(asString) : [];
  return { sandboxId: taken || undefined, discardedAliveSandboxIds: discarded };
}

function asArray(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function asOptionalString(value: unknown): string | undefined {
  return value == null ? undefined : asString(value);
}

function asString(value: unknown): string {
  if (typeof value === "string") return value;
  if (value instanceof Uint8Array) return new TextDecoder().decode(value);
  return String(value);
}

function parseInteger(value: unknown): number | undefined {
  if (value == null) return undefined;
  const parsed = Number(asString(value));
  return Number.isSafeInteger(parsed) ? parsed : undefined;
}

function isOne(value: unknown): boolean {
  return parseInteger(value) === 1;
}

function validateOwnerAndTtl(ownerId: string, ttlSeconds: number): void {
  if (!ownerId.trim()) throw new Error("ownerId must not be blank");
  requirePositive(ttlSeconds, "ttlSeconds");
}

function requireNonNegative(value: number, name: string): void {
  if (!Number.isFinite(value) || value < 0) throw new Error(`${name} must be non-negative`);
}

function requirePositive(value: number, name: string): void {
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${name} must be positive`);
}

const TAKE_IDLE_SCRIPT = `
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local min_remaining_ttl_ms = tonumber(ARGV[1]) or 0
local cutoff_ms = now_ms + min_remaining_ttl_ms
local discarded_alive = {}
while true do
  local sandbox_id = redis.call('LPOP', KEYS[1])
  if not sandbox_id then
    if #discarded_alive == 0 then return nil end
    return {'', discarded_alive}
  end
  local expires_at = redis.call('HGET', KEYS[2], sandbox_id)
  if expires_at then
    redis.call('HDEL', KEYS[2], sandbox_id)
    local exp = tonumber(expires_at)
    if exp > cutoff_ms then return {sandbox_id, discarded_alive} end
    if exp > now_ms then table.insert(discarded_alive, sandbox_id) end
  end
end`;

const PUT_IDLE_SCRIPT = `
if redis.call('GET', KEYS[3]) then return -1 end
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local expires_at = now_ms + tonumber(ARGV[2])
local current_expires_at = redis.call('HGET', KEYS[2], ARGV[1])
if current_expires_at and tonumber(current_expires_at) > now_ms then return 0 end
if not current_expires_at then redis.call('RPUSH', KEYS[1], ARGV[1]) end
redis.call('HSET', KEYS[2], ARGV[1], expires_at)
return 1`;

const ACQUIRE_LOCK_SCRIPT = `
if redis.call('GET', KEYS[2]) then return 0 end
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then return 1 end
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0`;

const RENEW_LOCK_SCRIPT = `
if redis.call('GET', KEYS[2]) then return 0 end
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0`;

const SET_VALUE_SCRIPT = `
if redis.call('GET', KEYS[2]) then return -1 end
redis.call('SET', KEYS[1], ARGV[1])
return 1`;

const BEGIN_DESTROY_SCRIPT = `
local destroy_state = redis.call('GET', KEYS[1])
if destroy_state == ARGV[2] then return -1 end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[3])
return 1`;

const RELEASE_LOCK_SCRIPT = `
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`;

const REAP_EXPIRED_SCRIPT = `
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local min_remaining_ttl_ms = tonumber(ARGV[1]) or 0
local cutoff_ms = now_ms + min_remaining_ttl_ms
local discarded_alive = {}
local entries = redis.call('HGETALL', KEYS[2])
for i = 1, #entries, 2 do
  local sandbox_id = entries[i]
  local exp = tonumber(entries[i + 1])
  if exp <= cutoff_ms then
    redis.call('HDEL', KEYS[2], sandbox_id)
    redis.call('LREM', KEYS[1], 0, sandbox_id)
    if exp > now_ms then table.insert(discarded_alive, sandbox_id) end
  end
end
return discarded_alive`;
