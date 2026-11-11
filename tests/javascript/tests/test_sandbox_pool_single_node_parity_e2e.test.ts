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

import { expect, test } from "vitest";

import {
  AcquirePolicy,
  InMemoryPoolStateStore,
  PoolAcquireFailedException,
  PoolEmptyException,
  PoolLifecycleState,
  PoolNotRunningException,
  PoolState,
  PoolStateStoreUnavailableException,
  SandboxManager,
  type Sandbox,
} from "@alibaba-group/opensandbox";

import { createConnectionConfig } from "./base_e2e.ts";
import {
  cleanupPool,
  createPool,
  deferred,
  eventually,
  overrideStore,
  POOL_TEST_TIMEOUT,
  sleep,
  taggedSandboxIds,
  uniquePoolName,
} from "./pool_e2e_helpers.ts";

test("lifecycle is idempotent and a stopped pool can resize then restart", async () => {
  const poolName = uniquePoolName("lifecycle");
  const pool = createPool(poolName);
  try {
    await Promise.all([pool.start(), pool.start()]);
    await eventually("initial idle", async () => (await pool.snapshot()).idleCount === 1);
    await Promise.all([pool.shutdown(true), pool.shutdown(true)]);
    expect((await pool.snapshot()).lifecycleState).toBe(PoolLifecycleState.STOPPED);
    await expect(pool.acquire()).rejects.toBeInstanceOf(PoolNotRunningException);
    await pool.resize(2);
    await pool.start();
    await eventually("rewarm after restart", async () => (await pool.snapshot()).idleCount === 2);
    const snapshot = await pool.snapshot();
    expect(snapshot.maxIdle).toBe(2);
    expect(snapshot.state).toBe(PoolState.HEALTHY);
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("concurrent acquire returns each warmed sandbox at most once", async () => {
  const poolName = uniquePoolName("concurrent-acquire");
  const pool = createPool(poolName, { maxIdle: 4, warmupCreateQps: 4, warmupConcurrency: 4 });
  const acquired: Sandbox[] = [];
  try {
    await pool.start();
    await eventually("four idle sandboxes", async () => (await pool.snapshot()).idleCount === 4);
    await pool.resize(0);
    acquired.push(...await Promise.all(Array.from({ length: 4 }, () =>
      pool.acquire({ policy: AcquirePolicy.FAIL_FAST }),
    )));
    expect(new Set(acquired.map((sandbox) => String(sandbox.id))).size).toBe(4);
    await expect(pool.acquire({ policy: AcquirePolicy.FAIL_FAST })).rejects.toBeInstanceOf(PoolEmptyException);
  } finally {
    await Promise.all(acquired.map(async (sandbox) => {
      await sandbox.kill().catch(() => undefined);
      await sandbox.close().catch(() => undefined);
    }));
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("releaseAllIdle works after stop and removes remote tagged sandboxes", async () => {
  const poolName = uniquePoolName("release-stopped");
  const pool = createPool(poolName, { maxIdle: 3, warmupCreateQps: 3, warmupConcurrency: 3 });
  try {
    await pool.start();
    await eventually("three idle sandboxes", async () => (await pool.snapshot()).idleCount === 3);
    await pool.shutdown(true);
    expect(await pool.releaseAllIdle(3)).toBe(3);
    await eventually("remote idle deletion", async () => (await taggedSandboxIds(poolName)).length === 0);
    expect((await pool.snapshot()).idleCount).toBe(0);
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("store failure degrades one pool while direct-create fallback and another pool stay healthy", async () => {
  const brokenName = uniquePoolName("degraded");
  const healthyName = uniquePoolName("healthy-isolation");
  const delegate = new InMemoryPoolStateStore();
  let broken = false;
  const store = overrideStore(delegate, {
    getDestroyState: async (poolName) => {
      if (broken) throw new Error("injected store outage");
      return await delegate.getDestroyState(poolName);
    },
    tryAcquirePrimaryLock: async (poolName, ownerId, ttlSeconds) => {
      if (broken) throw new Error("injected store outage");
      return await delegate.tryAcquirePrimaryLock(poolName, ownerId, ttlSeconds);
    },
  });
  const degraded = createPool(brokenName, { maxIdle: 0, stateStore: store, degradedThreshold: 2 });
  const healthy = createPool(healthyName);
  let direct: Sandbox | undefined;
  try {
    await Promise.all([degraded.start(), healthy.start()]);
    await eventually("healthy pool idle", async () => (await healthy.snapshot()).idleCount === 1);
    broken = true;
    await eventually("broken pool degraded", async () => (await degraded.snapshot()).state === PoolState.DEGRADED, 10_000);
    await expect(degraded.acquire({ policy: AcquirePolicy.FAIL_FAST }))
      .rejects.toBeInstanceOf(PoolStateStoreUnavailableException);
    direct = await degraded.acquire({ policy: AcquirePolicy.DIRECT_CREATE });
    expect(await direct.isHealthy()).toBe(true);
    expect((await healthy.snapshot()).state).toBe(PoolState.HEALTHY);
  } finally {
    broken = false;
    await direct?.kill().catch(() => undefined);
    await direct?.close().catch(() => undefined);
    await cleanupPool(brokenName, [degraded]);
    await cleanupPool(healthyName, [healthy]);
  }
}, POOL_TEST_TIMEOUT);

test("warmupConcurrency bounds post-create work and a slow warmup does not block a peer", async () => {
  const poolName = uniquePoolName("warmup-concurrency");
  const release = deferred();
  const twoEntered = deferred();
  let active = 0;
  let maxActive = 0;
  let entered = 0;
  const pool = createPool(poolName, {
    maxIdle: 3,
    warmupCreateQps: 3,
    warmupConcurrency: 2,
    warmupSandboxPreparer: async () => {
      entered += 1;
      active += 1;
      maxActive = Math.max(maxActive, active);
      if (entered === 2) twoEntered.resolve(undefined);
      await release.promise;
      active -= 1;
    },
  });
  try {
    await pool.start();
    await twoEntered.promise;
    await sleep(300);
    expect(entered).toBe(2);
    expect(maxActive).toBe(2);
    release.resolve(undefined);
    await eventually("bounded warmup convergence", async () => (await pool.snapshot()).idleCount === 3);
    expect(entered).toBe(3);
  } finally {
    release.resolve(undefined);
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("one slow admitted warmup does not prevent a later peer from becoming idle", async () => {
  const poolName = uniquePoolName("slow-warmup-peer");
  const firstEntered = deferred();
  const releaseFirst = deferred();
  let firstSandboxId: string | undefined;
  const pool = createPool(poolName, {
    maxIdle: 2,
    warmupCreateQps: 2,
    warmupConcurrency: 2,
    warmupSandboxPreparer: async (sandbox) => {
      firstSandboxId ??= String(sandbox.id);
      if (String(sandbox.id) === firstSandboxId) {
        firstEntered.resolve(undefined);
        await releaseFirst.promise;
      }
    },
  });
  try {
    await pool.start();
    await firstEntered.promise;
    await eventually("later warmup published", async () => {
      const snapshot = await pool.snapshot();
      return snapshot.idleCount === 1 && snapshot.inFlightOperations === 1;
    });
    expect((await pool.snapshotIdleEntries())[0]?.sandboxId).not.toBe(firstSandboxId);
    releaseFirst.resolve(undefined);
    await eventually("both warmups published", async () => (await pool.snapshot()).idleCount === 2);
  } finally {
    releaseFirst.resolve(undefined);
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("retry-next-idle skips a stale sandbox and returns the next healthy idle", async () => {
  const poolName = uniquePoolName("retry-next");
  const pool = createPool(poolName, { maxIdle: 2, warmupCreateQps: 2, warmupConcurrency: 2 });
  const manager = SandboxManager.create({ connectionConfig: createConnectionConfig() });
  let acquired: Sandbox | undefined;
  try {
    await pool.start();
    await eventually("two idle sandboxes", async () => (await pool.snapshot()).idleCount === 2);
    await pool.resize(0);
    const entries = await pool.snapshotIdleEntries();
    await manager.killSandbox(entries[0]!.sandboxId);
    acquired = await pool.acquire({ policy: AcquirePolicy.RETRY_NEXT_IDLE });
    expect(String(acquired.id)).toBe(entries[1]!.sandboxId);
    expect(await acquired.isHealthy()).toBe(true);
  } finally {
    await acquired?.kill().catch(() => undefined);
    await acquired?.close().catch(() => undefined);
    await manager.close();
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("retry-next-idle-then-create falls through after all idle candidates are stale", async () => {
  const poolName = uniquePoolName("retry-create");
  const pool = createPool(poolName, { maxIdle: 2, warmupCreateQps: 2, warmupConcurrency: 2 });
  const manager = SandboxManager.create({ connectionConfig: createConnectionConfig() });
  let acquired: Sandbox | undefined;
  try {
    await pool.start();
    await eventually("two idle sandboxes", async () => (await pool.snapshot()).idleCount === 2);
    await pool.resize(0);
    const staleIds = new Set((await pool.snapshotIdleEntries()).map((entry) => entry.sandboxId));
    await Promise.all([...staleIds].map((id) => manager.killSandbox(id)));
    acquired = await pool.acquire({ policy: AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE });
    expect(staleIds.has(String(acquired.id))).toBe(false);
    expect(await acquired.isHealthy()).toBe(true);
  } finally {
    await acquired?.kill().catch(() => undefined);
    await acquired?.close().catch(() => undefined);
    await manager.close();
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("retry-next-idle raises after bounded retries when all candidates are stale", async () => {
  const poolName = uniquePoolName("retry-bounded");
  const pool = createPool(poolName, {
    maxIdle: 2,
    maxAcquireRetries: 2,
    warmupCreateQps: 2,
    warmupConcurrency: 2,
  });
  const manager = SandboxManager.create({ connectionConfig: createConnectionConfig() });
  try {
    await pool.start();
    await eventually("two idle sandboxes", async () => (await pool.snapshot()).idleCount === 2);
    await pool.resize(0);
    const entries = await pool.snapshotIdleEntries();
    await Promise.all(entries.map((entry) => manager.killSandbox(entry.sandboxId)));
    await expect(pool.acquire({ policy: AcquirePolicy.RETRY_NEXT_IDLE }))
      .rejects.toBeInstanceOf(PoolAcquireFailedException);
    expect((await pool.snapshot()).idleCount).toBe(0);
  } finally {
    await manager.close();
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);
