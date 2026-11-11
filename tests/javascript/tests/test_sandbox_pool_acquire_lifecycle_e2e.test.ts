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
  PoolAcquireFailedException,
  PoolNotRunningException,
  type Sandbox,
} from "@alibaba-group/opensandbox";

import {
  cleanupPool,
  createPool,
  deferred,
  eventually,
  POOL_TEST_TIMEOUT,
  taggedSandboxIds,
  uniquePoolName,
} from "./pool_e2e_helpers.ts";

test("cancelled acquire stops fallback and deletes the popped idle", async () => {
  const poolName = uniquePoolName("cancel-acquire");
  const entered = deferred();
  const release = deferred();
  const abortController = new AbortController();
  const pool = createPool(poolName, {
    acquireHealthCheck: async () => {
      entered.resolve(undefined);
      await release.promise;
      return true;
    },
  });

  try {
    await pool.start();
    await eventually("initial idle", async () => (await pool.snapshot()).idleCount === 1);
    const acquiring = pool.acquire({
      policy: AcquirePolicy.DIRECT_CREATE,
      sandboxTimeoutSeconds: 5 * 60,
      signal: abortController.signal,
    });
    await entered.promise;
    await pool.resize(0);
    const cancelled = new Error("cancelled by E2E");
    abortController.abort(cancelled);
    await expect(acquiring).rejects.toBe(cancelled);
    await eventually("cancelled candidate deletion", async () => (await taggedSandboxIds(poolName)).length === 0);
    expect((await pool.snapshot()).inFlightOperations).toBe(0);
  } finally {
    release.resolve(undefined);
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("retired acquire cannot consume idle warmed by a restarted run", async () => {
  const poolName = uniquePoolName("restart-acquire");
  const oldCheckEntered = deferred();
  const releaseOldCheck = deferred();
  let oldSandboxId: string | undefined;
  const pool = createPool(poolName, {
    maxAcquireRetries: 3,
    acquireHealthCheck: async (sandbox) => {
      if (String(sandbox.id) === oldSandboxId) {
        oldCheckEntered.resolve(undefined);
        await releaseOldCheck.promise;
        return false;
      }
      return true;
    },
  });
  let acquired: Sandbox | undefined;

  try {
    await pool.start();
    await eventually("old run idle", async () => (await pool.snapshot()).idleCount === 1);
    oldSandboxId = (await pool.snapshotIdleEntries())[0]?.sandboxId;
    const oldAcquire = pool.acquire({ policy: AcquirePolicy.RETRY_NEXT_IDLE });
    await oldCheckEntered.promise;

    await pool.shutdown(false);
    await pool.start();
    await eventually("new run replacement idle", async () => (await pool.snapshot()).idleCount === 1);
    const newSandboxId = (await pool.snapshotIdleEntries())[0]?.sandboxId;
    expect(newSandboxId).toBeTruthy();
    expect(newSandboxId).not.toBe(oldSandboxId);
    releaseOldCheck.resolve(undefined);

    await expect(oldAcquire).rejects.toBeInstanceOf(PoolNotRunningException);
    expect((await pool.snapshotIdleEntries())[0]?.sandboxId).toBe(newSandboxId);
    await pool.resize(0);
    acquired = await pool.acquire({ policy: AcquirePolicy.FAIL_FAST });
    expect(String(acquired.id)).toBe(newSandboxId);
  } finally {
    releaseOldCheck.resolve(undefined);
    await acquired?.kill().catch(() => undefined);
    await acquired?.close().catch(() => undefined);
    await cleanupPool(poolName, [pool]);
  }
}, 6 * 60_000);

test("acquire health-check failure deletes the popped idle and releases counters", async () => {
  const poolName = uniquePoolName("acquire-error");
  const expected = new Error("user acquire health check failed");
  const pool = createPool(poolName, {
    acquireReadyTimeoutSeconds: 0.5,
    acquireHealthCheck: async () => { throw expected; },
  });

  try {
    await pool.start();
    await eventually("idle before health error", async () => (await pool.snapshot()).idleCount === 1);
    await pool.resize(0);
    await expect(pool.acquire({ policy: AcquirePolicy.FAIL_FAST }))
      .rejects.toBeInstanceOf(PoolAcquireFailedException);
    await eventually("failed candidate deletion", async () => (await taggedSandboxIds(poolName)).length === 0);
    const snapshot = await pool.snapshot();
    expect(snapshot.idleCount).toBe(0);
    expect(snapshot.inFlightOperations).toBe(0);
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("warmup health-check error releases counters and allows replacement", async () => {
  const poolName = uniquePoolName("warmup-error");
  let healthChecks = 0;
  const pool = createPool(poolName, {
    warmupHealthCheck: async (sandbox) => {
      healthChecks += 1;
      if (healthChecks === 1) throw new Error("user warmup health check failed");
      return await sandbox.isHealthy();
    },
  });

  try {
    await pool.start();
    await eventually("replacement after warmup error", async () => {
      const snapshot = await pool.snapshot();
      return healthChecks >= 2 && snapshot.idleCount === 1 && snapshot.inFlightOperations === 0;
    });
    expect(await taggedSandboxIds(poolName)).toHaveLength(1);
    expect(await pool.snapshotIdleEntries()).toHaveLength(1);
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);
