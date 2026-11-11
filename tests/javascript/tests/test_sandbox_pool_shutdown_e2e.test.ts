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

import { PoolLifecycleState } from "@alibaba-group/opensandbox";

import {
  cleanupPool,
  createPool,
  deferred,
  eventually,
  POOL_TEST_TIMEOUT,
  sleep,
  taggedSandboxIds,
  uniquePoolName,
} from "./pool_e2e_helpers.ts";

test("graceful shutdown drains all admitted warmups without admitting new work", async () => {
  const poolName = uniquePoolName("graceful-all");
  const release = deferred();
  let entered = 0;
  const allEntered = deferred();
  const pool = createPool(poolName, {
    maxIdle: 3,
    warmupCreateQps: 3,
    warmupConcurrency: 3,
    drainTimeoutSeconds: 60,
    warmupSandboxPreparer: async () => {
      entered += 1;
      if (entered === 3) allEntered.resolve(undefined);
      await release.promise;
    },
  });

  try {
    await pool.start();
    await allEntered.promise;
    const shutdown = pool.shutdown(true);
    await sleep(100);
    expect((await pool.snapshot()).lifecycleState).toBe(PoolLifecycleState.DRAINING);
    release.resolve(undefined);
    await shutdown;
    const snapshot = await pool.snapshot();
    expect(snapshot.lifecycleState).toBe(PoolLifecycleState.STOPPED);
    expect(snapshot.idleCount).toBe(3);
    expect(snapshot.inFlightOperations).toBe(0);
    await sleep(1_200);
    expect(await taggedSandboxIds(poolName)).toHaveLength(3);
  } finally {
    release.resolve(undefined);
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("forced shutdown aborts a blocked preparer and deletes its sandbox", async () => {
  const poolName = uniquePoolName("forced-preparer");
  const entered = deferred();
  const never = deferred();
  const pool = createPool(poolName, {
    drainTimeoutSeconds: 0.2,
    warmupSandboxPreparer: async () => {
      entered.resolve(undefined);
      await never.promise;
    },
  });

  try {
    await pool.start();
    await entered.promise;
    expect(await taggedSandboxIds(poolName)).toHaveLength(1);
    await pool.shutdown(false);
    expect((await pool.snapshot()).lifecycleState).toBe(PoolLifecycleState.STOPPED);
    await eventually("forced warmup cleanup", async () => (await taggedSandboxIds(poolName)).length === 0);
    expect((await pool.snapshot()).inFlightOperations).toBe(0);
  } finally {
    never.resolve(undefined);
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("restart fences a late warmup from the retired run", async () => {
  const poolName = uniquePoolName("restart-warmup");
  const oldEntered = deferred();
  const releaseOld = deferred();
  let firstSandboxId: string | undefined;
  const pool = createPool(poolName, {
    warmupSandboxPreparer: async (sandbox) => {
      firstSandboxId ??= String(sandbox.id);
      if (String(sandbox.id) === firstSandboxId) {
        oldEntered.resolve(undefined);
        await releaseOld.promise;
      }
    },
  });

  try {
    await pool.start();
    await oldEntered.promise;
    await pool.shutdown(false);
    await pool.start();
    await eventually("new run idle", async () => (await pool.snapshot()).idleCount === 1);
    const newSandboxId = (await pool.snapshotIdleEntries())[0]?.sandboxId;
    expect(newSandboxId).toBeTruthy();
    expect(newSandboxId).not.toBe(firstSandboxId);
    releaseOld.resolve(undefined);
    await eventually("retired warmup cleanup", async () => {
      const ids = await taggedSandboxIds(poolName);
      return ids.length === 1 && ids[0] === newSandboxId;
    });
    expect((await pool.snapshotIdleEntries())[0]?.sandboxId).toBe(newSandboxId);
  } finally {
    releaseOld.resolve(undefined);
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);
