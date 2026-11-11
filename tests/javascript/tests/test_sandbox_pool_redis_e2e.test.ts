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

import { createClient } from "redis";
import { expect, test } from "vitest";

import {
  AcquirePolicy,
  PoolEmptyException,
  PoolDestroyedException,
  PoolLifecycleState,
  Sandbox,
  SandboxPool,
  SandboxPoolManager,
  type SandboxPoolOptions,
} from "@alibaba-group/opensandbox";
import { RedisPoolStateStore } from "@alibaba-group/opensandbox/pool-redis";

import { createConnectionConfig, getSandboxImage } from "./base_e2e.ts";
import { deferred, overrideStore, sleep, taggedSandboxIds } from "./pool_e2e_helpers.ts";

const redisUrl = process.env.OPENSANDBOX_TEST_REDIS_URL;
const redisTest = redisUrl ? test : test.skip;

async function eventually(check: () => Promise<boolean>, timeoutMs = 120_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error("distributed pool did not converge before timeout");
}

redisTest("Redis pool coordinates acquire, resize, failover fencing, and destroy", async () => {
  const poolName = `js-pool-redis-${Math.random().toString(16).slice(2, 10)}`;
  const connectionConfig = createConnectionConfig();
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const storeA = new RedisPoolStateStore({ client: redisA });
  const storeB = new RedisPoolStateStore({ client: redisB });
  const common = {
    poolName,
    maxIdle: 1,
    connectionConfig,
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    warmupReadyTimeoutSeconds: 60,
  };
  const poolA = SandboxPool.create({ ...common, ownerId: `${poolName}-a`, stateStore: storeA });
  const poolB = SandboxPool.create({ ...common, ownerId: `${poolName}-b`, stateStore: storeB });
  const manager = new SandboxPoolManager({ stateStore: storeA, connectionConfig });
  let acquired: Sandbox | undefined;

  try {
    await Promise.all([poolA.start(), poolB.start()]);
    await eventually(async () => (await poolA.snapshot()).idleCount === 1);

    acquired = await poolB.acquire({
      policy: AcquirePolicy.FAIL_FAST,
      sandboxTimeoutSeconds: 5 * 60,
    });
    expect(await acquired.isHealthy()).toBe(true);
    await eventually(async () => (await poolA.snapshot()).idleCount === 1);

    await poolB.resize(2);
    await eventually(async () => (await poolA.snapshot()).idleCount === 2);

    const result = await manager.destroy(poolName, { tombstoneTtlSeconds: 60 });
    expect(result.drainedIdleCount).toBe(2);
    expect(result.killedIdleCount).toBe(2);
    await expect(poolA.acquire()).rejects.toBeInstanceOf(PoolDestroyedException);
    await expect(poolB.acquire()).rejects.toBeInstanceOf(PoolDestroyedException);
    await eventually(async () =>
      (await poolA.snapshot()).lifecycleState === PoolLifecycleState.STOPPED &&
      (await poolB.snapshot()).lifecycleState === PoolLifecycleState.STOPPED,
    );
  } finally {
    await poolA.shutdown(false).catch(() => undefined);
    await poolB.shutdown(false).catch(() => undefined);
    await acquired?.kill().catch(() => undefined);
    await acquired?.close().catch(() => undefined);
    await Promise.all([
      redisA.close().catch(() => undefined),
      redisB.close().catch(() => undefined),
    ]);
  }
}, 10 * 60_000);

redisTest("Redis secondary resize is applied by the primary reconciler", async () => {
  const poolName = `js-pool-redis-resize-${Math.random().toString(16).slice(2, 10)}`;
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const common = redisPoolOptions(poolName);
  const poolA = SandboxPool.create({ ...common, ownerId: `${poolName}-a`, stateStore: new RedisPoolStateStore({ client: redisA }) });
  const poolB = SandboxPool.create({ ...common, ownerId: `${poolName}-b`, stateStore: new RedisPoolStateStore({ client: redisB }) });
  try {
    await poolA.start();
    await eventually(async () => (await poolA.snapshot()).idleCount === 1);
    await poolB.start();
    await poolB.resize(2);
    await eventually(async () => (await poolA.snapshot()).idleCount === 2);
    expect((await poolA.snapshot()).maxIdle).toBe(2);
  } finally {
    await cleanupRedisPools([poolA, poolB]);
    await Promise.all([redisA.close().catch(() => undefined), redisB.close().catch(() => undefined)]);
  }
}, 10 * 60_000);

redisTest("Redis follower takes over after the primary shuts down", async () => {
  const poolName = `js-pool-redis-failover-${Math.random().toString(16).slice(2, 10)}`;
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const common = redisPoolOptions(poolName);
  const poolA = SandboxPool.create({ ...common, ownerId: `${poolName}-a`, stateStore: new RedisPoolStateStore({ client: redisA }) });
  const poolB = SandboxPool.create({ ...common, ownerId: `${poolName}-b`, stateStore: new RedisPoolStateStore({ client: redisB }) });
  try {
    await poolA.start();
    await eventually(async () => (await poolA.snapshot()).idleCount === 1);
    await poolB.start();
    await poolA.shutdown(true);
    await poolB.resize(2);
    await eventually(async () => (await poolB.snapshot()).idleCount === 2);
    expect((await poolB.snapshot()).lifecycleState).toBe(PoolLifecycleState.RUNNING);
  } finally {
    await cleanupRedisPools([poolA, poolB]);
    await Promise.all([redisA.close().catch(() => undefined), redisB.close().catch(() => undefined)]);
  }
}, 10 * 60_000);

redisTest("Redis primary heartbeat survives a blocked warmup stage", async () => {
  const poolName = `js-pool-redis-heartbeat-${Math.random().toString(16).slice(2, 10)}`;
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const entered = deferred();
  const release = deferred();
  const common = redisPoolOptions(poolName, {
    primaryLockTtlSeconds: 2,
    warmupSandboxPreparer: async () => {
      entered.resolve(undefined);
      await release.promise;
    },
  });
  const poolA = SandboxPool.create({ ...common, ownerId: `${poolName}-a`, stateStore: new RedisPoolStateStore({ client: redisA }) });
  const poolB = SandboxPool.create({ ...common, ownerId: `${poolName}-b`, stateStore: new RedisPoolStateStore({ client: redisB }) });
  try {
    await poolA.start();
    await entered.promise;
    await poolB.start();
    await sleep(4_000);
    expect(await taggedSandboxIds(poolName)).toHaveLength(1);
    release.resolve(undefined);
    await eventually(async () => (await poolA.snapshot()).idleCount === 1);
  } finally {
    release.resolve(undefined);
    await cleanupRedisPools([poolA, poolB]);
    await Promise.all([redisA.close().catch(() => undefined), redisB.close().catch(() => undefined)]);
  }
}, 10 * 60_000);

redisTest("Redis concurrent cross-node acquire never returns one idle twice", async () => {
  const poolName = `js-pool-redis-acquire-${Math.random().toString(16).slice(2, 10)}`;
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const common = redisPoolOptions(poolName, { maxIdle: 2, warmupCreateQps: 2, warmupConcurrency: 2 });
  const poolA = SandboxPool.create({ ...common, ownerId: `${poolName}-a`, stateStore: new RedisPoolStateStore({ client: redisA }) });
  const poolB = SandboxPool.create({ ...common, ownerId: `${poolName}-b`, stateStore: new RedisPoolStateStore({ client: redisB }) });
  const acquired: Sandbox[] = [];
  try {
    await Promise.all([poolA.start(), poolB.start()]);
    await eventually(async () => (await poolA.snapshot()).idleCount === 2);
    await poolA.resize(0);
    const results = await Promise.allSettled([
      poolA.acquire({ policy: AcquirePolicy.FAIL_FAST }),
      poolA.acquire({ policy: AcquirePolicy.FAIL_FAST }),
      poolB.acquire({ policy: AcquirePolicy.FAIL_FAST }),
      poolB.acquire({ policy: AcquirePolicy.FAIL_FAST }),
    ]);
    acquired.push(...results.flatMap((result) => result.status === "fulfilled" ? [result.value] : []));
    expect(acquired).toHaveLength(2);
    expect(new Set(acquired.map((sandbox) => String(sandbox.id))).size).toBe(2);
    expect(results.filter((result) => result.status === "rejected" && result.reason instanceof PoolEmptyException)).toHaveLength(2);
  } finally {
    await Promise.all(acquired.map(async (sandbox) => {
      await sandbox.kill().catch(() => undefined);
      await sandbox.close().catch(() => undefined);
    }));
    await cleanupRedisPools([poolA, poolB]);
    await Promise.all([redisA.close().catch(() => undefined), redisB.close().catch(() => undefined)]);
  }
}, 10 * 60_000);

redisTest("Redis atomic take has one winner under contention", async () => {
  const poolName = `js-pool-redis-atomic-${Math.random().toString(16).slice(2, 10)}`;
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const storeA = new RedisPoolStateStore({ client: redisA });
  const storeB = new RedisPoolStateStore({ client: redisB });
  try {
    await storeA.setIdleEntryTtl(poolName, 60);
    await storeA.putIdle(poolName, "only-idle");
    const results = await Promise.all([storeA.tryTakeIdle(poolName), storeB.tryTakeIdle(poolName)]);
    expect(results.filter((id) => id === "only-idle")).toHaveLength(1);
    expect(results.filter((id) => id === undefined)).toHaveLength(1);
  } finally {
    await storeA.clearPoolState(poolName).catch(() => undefined);
    await Promise.all([redisA.close().catch(() => undefined), redisB.close().catch(() => undefined)]);
  }
}, 2 * 60_000);

redisTest("Redis snapshot retains expired entries until take reaps them", async () => {
  const poolName = `js-pool-redis-expired-${Math.random().toString(16).slice(2, 10)}`;
  const redis = createClient({ url: redisUrl });
  await redis.connect();
  const store = new RedisPoolStateStore({ client: redis });
  try {
    await store.setIdleEntryTtl(poolName, 0.1);
    await store.putIdle(poolName, "expired-idle");
    await sleep(200);
    expect((await store.snapshotCounters(poolName)).idleCount).toBe(1);
    expect(await store.tryTakeIdle(poolName)).toBeUndefined();
    expect((await store.snapshotCounters(poolName)).idleCount).toBe(0);
  } finally {
    await store.clearPoolState(poolName).catch(() => undefined);
    await redis.close().catch(() => undefined);
  }
}, 2 * 60_000);

redisTest("Redis lost-lock commit drops the orphan and replenishes", async () => {
  const poolName = `js-pool-redis-lost-lock-${Math.random().toString(16).slice(2, 10)}`;
  const redis = createClient({ url: redisUrl });
  await redis.connect();
  const delegate = new RedisPoolStateStore({ client: redis });
  const entered = deferred();
  const release = deferred();
  let rejectNextRenew = false;
  let failedSandboxId: string | undefined;
  const stateStore = overrideStore(delegate, {
    renewPrimaryLock: async (name, owner, ttl) => {
      if (rejectNextRenew) {
        rejectNextRenew = false;
        return false;
      }
      return await delegate.renewPrimaryLock(name, owner, ttl);
    },
  });
  const pool = SandboxPool.create({
    ...redisPoolOptions(poolName, {
      warmupSandboxPreparer: async (sandbox) => {
        if (!failedSandboxId) {
          failedSandboxId = String(sandbox.id);
          entered.resolve(undefined);
          await release.promise;
        }
      },
    }),
    stateStore,
  });
  try {
    await pool.start();
    await entered.promise;
    rejectNextRenew = true;
    release.resolve(undefined);
    await eventually(async () => {
      const ids = await taggedSandboxIds(poolName);
      return failedSandboxId !== undefined && !ids.includes(failedSandboxId) &&
        ids.length === 1 && (await pool.snapshot()).idleCount === 1;
    });
  } finally {
    release.resolve(undefined);
    await cleanupRedisPools([pool]);
    await redis.close().catch(() => undefined);
  }
}, 10 * 60_000);

redisTest("Redis start overwrites a stale shared maxIdle target", async () => {
  const poolName = `js-pool-redis-stale-max-${Math.random().toString(16).slice(2, 10)}`;
  const redis = createClient({ url: redisUrl });
  await redis.connect();
  const store = new RedisPoolStateStore({ client: redis });
  await store.setMaxIdle(poolName, 7);
  const pool = SandboxPool.create({ ...redisPoolOptions(poolName, { maxIdle: 0 }), stateStore: store });
  try {
    await pool.start();
    expect((await pool.snapshot()).maxIdle).toBe(0);
    expect(await store.getMaxIdle(poolName)).toBe(0);
  } finally {
    await cleanupRedisPools([pool]);
    await store.clearPoolState(poolName).catch(() => undefined);
    await redis.close().catch(() => undefined);
  }
}, 2 * 60_000);

redisTest("Redis destroying fence blocks all live pool nodes", async () => {
  const poolName = `js-pool-redis-destroying-${Math.random().toString(16).slice(2, 10)}`;
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const storeA = new RedisPoolStateStore({ client: redisA });
  const storeB = new RedisPoolStateStore({ client: redisB });
  const common = redisPoolOptions(poolName, { maxIdle: 0 });
  const poolA = SandboxPool.create({ ...common, ownerId: `${poolName}-a`, stateStore: storeA });
  const poolB = SandboxPool.create({ ...common, ownerId: `${poolName}-b`, stateStore: storeB });
  const manager = new SandboxPoolManager({ stateStore: storeA, connectionConfig: createConnectionConfig() });
  try {
    await Promise.all([poolA.start(), poolB.start()]);
    await storeA.beginDestroy(poolName, `${poolName}-destroyer`);
    await expect(poolA.acquire()).rejects.toBeInstanceOf(PoolDestroyedException);
    await expect(poolB.acquire()).rejects.toBeInstanceOf(PoolDestroyedException);
    await eventually(async () =>
      (await poolA.snapshot()).lifecycleState === PoolLifecycleState.STOPPED &&
      (await poolB.snapshot()).lifecycleState === PoolLifecycleState.STOPPED,
    );
    await manager.destroy(poolName, { tombstoneTtlSeconds: 60 });
  } finally {
    await cleanupRedisPools([poolA, poolB]);
    await Promise.all([redisA.close().catch(() => undefined), redisB.close().catch(() => undefined)]);
  }
}, 5 * 60_000);

redisTest("Redis stale idle is removed before direct-create fallback", async () => {
  const poolName = `js-pool-redis-stale-${Math.random().toString(16).slice(2, 10)}`;
  const redis = createClient({ url: redisUrl });
  await redis.connect();
  const pool = SandboxPool.create({
    ...redisPoolOptions(poolName),
    stateStore: new RedisPoolStateStore({ client: redis }),
  });
  let acquired: Sandbox | undefined;
  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    await pool.resize(0);
    const staleId = (await pool.snapshotIdleEntries())[0]!.sandboxId;
    const stale = await Sandbox.connect({
      sandboxId: staleId,
      connectionConfig: createConnectionConfig(),
      skipHealthCheck: true,
    });
    await stale.kill();
    await stale.close();
    acquired = await pool.acquire({ policy: AcquirePolicy.DIRECT_CREATE });
    expect(String(acquired.id)).not.toBe(staleId);
    expect(await acquired.isHealthy()).toBe(true);
    expect((await pool.snapshot()).idleCount).toBe(0);
  } finally {
    await acquired?.kill().catch(() => undefined);
    await acquired?.close().catch(() => undefined);
    await cleanupRedisPools([pool]);
    await redis.close().catch(() => undefined);
  }
}, 10 * 60_000);

redisTest("Redis failover fences a late warmup from the retired primary", async () => {
  const poolName = `js-pool-redis-late-primary-${Math.random().toString(16).slice(2, 10)}`;
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const creatorEntered = deferred();
  const releaseCreator = deferred();
  let retiredSandboxId: string | undefined;
  const common = redisPoolOptions(poolName);
  const poolA = SandboxPool.create({
    ...common,
    ownerId: `${poolName}-a`,
    stateStore: new RedisPoolStateStore({ client: redisA }),
    sandboxCreator: async (context) => {
      creatorEntered.resolve(undefined);
      await releaseCreator.promise;
      const sandbox = await Sandbox.create({
        ...context.creationSpec,
        connectionConfig: context.createConnectionConfig,
        timeoutSeconds: context.idleTimeoutSeconds,
        skipHealthCheck: true,
      });
      retiredSandboxId = String(sandbox.id);
      return sandbox;
    },
  });
  const poolB = SandboxPool.create({
    ...common,
    ownerId: `${poolName}-b`,
    stateStore: new RedisPoolStateStore({ client: redisB }),
  });
  try {
    await poolA.start();
    await creatorEntered.promise;
    await poolA.shutdown(false);
    await poolB.start();
    await eventually(async () => (await poolB.snapshot()).idleCount === 1);
    const activeId = (await poolB.snapshotIdleEntries())[0]!.sandboxId;
    releaseCreator.resolve(undefined);
    await eventually(async () => {
      const ids = await taggedSandboxIds(poolName);
      return retiredSandboxId !== undefined && !ids.includes(retiredSandboxId) &&
        ids.length === 1 && ids[0] === activeId;
    });
  } finally {
    releaseCreator.resolve(undefined);
    await cleanupRedisPools([poolA, poolB]);
    await Promise.all([redisA.close().catch(() => undefined), redisB.close().catch(() => undefined)]);
  }
}, 10 * 60_000);

function redisPoolOptions(
  poolName: string,
  overrides: Partial<SandboxPoolOptions> = {},
): Parameters<typeof SandboxPool.create>[0] {
  return {
    poolName,
    maxIdle: 1,
    connectionConfig: createConnectionConfig(),
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName, suite: "sandbox-pool-redis-javascript-e2e" },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    warmupReadyTimeoutSeconds: 60,
    ...overrides,
  };
}

async function cleanupRedisPools(pools: SandboxPool[]): Promise<void> {
  for (const pool of pools) await pool.resize(0).catch(() => undefined);
  for (const pool of pools) await pool.releaseAllIdle(8).catch(() => undefined);
  for (const pool of pools) await pool.shutdown(false).catch(() => undefined);
}
