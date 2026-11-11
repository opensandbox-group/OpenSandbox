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

import type {
  IdleEntry,
  PoolStateStore,
  ReapResult,
  StoreCounters,
  TakeIdleResult,
} from "./poolTypes.js";
import { PoolDestroyState } from "./poolTypes.js";
import { PoolDestroyedException } from "./core/exceptions.js";

const DEFAULT_IDLE_TTL_SECONDS = 24 * 60 * 60;

interface PrimaryLock {
  ownerId: string;
  expiresAtMs: number;
}

interface PoolState {
  idleById: Map<string, IdleEntry>;
  idleOrder: string[];
  lock?: PrimaryLock;
  idleTtlSeconds: number;
}

interface DestroyStateEntry {
  state: PoolDestroyState;
  ownerId: string;
  expiresAtMs?: number;
}

/** Process-local state store. Calls are atomic within one JavaScript runtime. */
export class InMemoryPoolStateStore implements PoolStateStore {
  private readonly pools = new Map<string, PoolState>();
  private readonly destroyStates = new Map<string, DestroyStateEntry>();

  private state(poolName: string): PoolState {
    let state = this.pools.get(poolName);
    if (!state) {
      state = {
        idleById: new Map(),
        idleOrder: [],
        idleTtlSeconds: DEFAULT_IDLE_TTL_SECONDS,
      };
      this.pools.set(poolName, state);
    }
    return state;
  }

  async tryTakeIdle(poolName: string): Promise<string | undefined> {
    return (await this.tryTakeIdleWithMinTtl(poolName, 0)).sandboxId;
  }

  async tryTakeIdleWithMinTtl(
    poolName: string,
    minRemainingTtlSeconds: number,
  ): Promise<TakeIdleResult> {
    const state = this.state(poolName);
    const now = Date.now();
    const result: TakeIdleResult = { discardedAliveSandboxIds: [] };

    while (state.idleOrder.length > 0) {
      const sandboxId = state.idleOrder.shift()!;
      const entry = state.idleById.get(sandboxId);
      if (!entry) continue;
      state.idleById.delete(sandboxId);

      const remainingMs = entry.expiresAt.getTime() - now;
      if (remainingMs <= 0) continue;
      if (remainingMs < minRemainingTtlSeconds * 1000) {
        result.discardedAliveSandboxIds.push(sandboxId);
        continue;
      }
      result.sandboxId = sandboxId;
      return result;
    }
    return result;
  }

  async putIdle(poolName: string, sandboxId: string): Promise<void> {
    if (!sandboxId.trim()) throw new Error("sandboxId must not be blank");
    this.rejectIfDestroyed(poolName);
    const state = this.state(poolName);
    const existing = state.idleById.get(sandboxId);
    if (existing && existing.expiresAt.getTime() > Date.now()) return;
    if (existing) state.idleOrder = state.idleOrder.filter((id) => id !== sandboxId);

    state.idleById.set(sandboxId, {
      sandboxId,
      expiresAt: new Date(Date.now() + state.idleTtlSeconds * 1000),
    });
    state.idleOrder.push(sandboxId);
  }

  async removeIdle(poolName: string, sandboxId: string): Promise<void> {
    const state = this.state(poolName);
    state.idleById.delete(sandboxId);
    this.compact(state);
  }

  async tryAcquirePrimaryLock(
    poolName: string,
    ownerId: string,
    ttlSeconds: number,
  ): Promise<boolean> {
    if ((await this.getDestroyState(poolName)) !== PoolDestroyState.ACTIVE) return false;
    const state = this.state(poolName);
    const now = Date.now();
    if (state.lock && state.lock.ownerId !== ownerId && state.lock.expiresAtMs > now) return false;
    state.lock = { ownerId, expiresAtMs: now + ttlSeconds * 1000 };
    return true;
  }

  async renewPrimaryLock(
    poolName: string,
    ownerId: string,
    ttlSeconds: number,
  ): Promise<boolean> {
    if ((await this.getDestroyState(poolName)) !== PoolDestroyState.ACTIVE) return false;
    const state = this.state(poolName);
    const now = Date.now();
    if (state.lock?.ownerId !== ownerId) return false;
    if (state.lock.expiresAtMs <= now) {
      state.lock = undefined;
      return false;
    }
    state.lock.expiresAtMs = now + ttlSeconds * 1000;
    return true;
  }

  async releasePrimaryLock(poolName: string, ownerId: string): Promise<void> {
    const state = this.state(poolName);
    if (state.lock?.ownerId === ownerId) state.lock = undefined;
  }

  async reapExpiredIdle(poolName: string, now: Date): Promise<void> {
    await this.reapExpiredIdleWithMinTtl(poolName, now, 0);
  }

  async reapExpiredIdleWithMinTtl(
    poolName: string,
    now: Date,
    minRemainingTtlSeconds: number,
  ): Promise<ReapResult> {
    const state = this.state(poolName);
    const discardedAliveSandboxIds: string[] = [];
    const thresholdMs = minRemainingTtlSeconds * 1000;
    for (const [sandboxId, entry] of state.idleById) {
      const remainingMs = entry.expiresAt.getTime() - now.getTime();
      if (remainingMs <= 0 || remainingMs < thresholdMs) {
        state.idleById.delete(sandboxId);
        if (remainingMs > 0) discardedAliveSandboxIds.push(sandboxId);
      }
    }
    this.compact(state);
    return { discardedAliveSandboxIds };
  }

  async snapshotCounters(poolName: string): Promise<StoreCounters> {
    await this.reapExpiredIdle(poolName, new Date());
    return { idleCount: this.state(poolName).idleById.size };
  }

  async snapshotIdleEntries(poolName: string): Promise<IdleEntry[]> {
    await this.reapExpiredIdle(poolName, new Date());
    const state = this.state(poolName);
    return state.idleOrder.flatMap((sandboxId) => {
      const entry = state.idleById.get(sandboxId);
      return entry ? [{ ...entry, expiresAt: new Date(entry.expiresAt) }] : [];
    });
  }

  async getMaxIdle(_poolName: string): Promise<undefined> {
    return undefined;
  }

  async setMaxIdle(poolName: string, maxIdle: number): Promise<void> {
    if (!Number.isInteger(maxIdle) || maxIdle < 0) {
      throw new Error("maxIdle must be a non-negative integer");
    }
    this.rejectIfDestroyed(poolName);
    // Process-local pools keep their target in SandboxPool itself. Returning
    // no shared target keeps separate pool instances isolated, matching the
    // Kotlin in-memory store contract.
  }

  async setIdleEntryTtl(poolName: string, ttlSeconds: number): Promise<void> {
    if (!Number.isFinite(ttlSeconds) || ttlSeconds <= 0) {
      throw new Error("ttlSeconds must be a positive number");
    }
    this.rejectIfDestroyed(poolName);
    this.state(poolName).idleTtlSeconds = ttlSeconds;
  }

  async getDestroyState(poolName: string): Promise<PoolDestroyState> {
    const entry = this.destroyStates.get(poolName);
    if (!entry) return PoolDestroyState.ACTIVE;
    if (entry.expiresAtMs !== undefined && entry.expiresAtMs <= Date.now()) {
      this.destroyStates.delete(poolName);
      return PoolDestroyState.ACTIVE;
    }
    return entry.state;
  }

  async beginDestroy(poolName: string, ownerId: string): Promise<void> {
    if (!ownerId.trim()) throw new Error("ownerId must not be blank");
    const state = await this.getDestroyState(poolName);
    if (state === PoolDestroyState.DESTROYED) {
      throw new PoolDestroyedException(poolName, state);
    }
    this.destroyStates.set(poolName, { state: PoolDestroyState.DESTROYING, ownerId });
  }

  async clearPoolState(poolName: string): Promise<void> {
    this.pools.delete(poolName);
  }

  async markDestroyed(
    poolName: string,
    ownerId: string,
    tombstoneTtlSeconds?: number | null,
  ): Promise<void> {
    if (!ownerId.trim()) throw new Error("ownerId must not be blank");
    if (tombstoneTtlSeconds != null && (!Number.isFinite(tombstoneTtlSeconds) || tombstoneTtlSeconds <= 0)) {
      throw new Error("tombstoneTtlSeconds must be positive when set");
    }
    this.destroyStates.set(poolName, {
      state: PoolDestroyState.DESTROYED,
      ownerId,
      expiresAtMs: tombstoneTtlSeconds == null ? undefined : Date.now() + tombstoneTtlSeconds * 1000,
    });
  }

  private compact(state: PoolState): void {
    state.idleOrder = state.idleOrder.filter((id) => state.idleById.has(id));
  }

  private rejectIfDestroyed(poolName: string): void {
    const entry = this.destroyStates.get(poolName);
    if (!entry) return;
    if (entry.expiresAtMs !== undefined && entry.expiresAtMs <= Date.now()) {
      this.destroyStates.delete(poolName);
      return;
    }
    throw new PoolDestroyedException(poolName, entry.state);
  }
}
