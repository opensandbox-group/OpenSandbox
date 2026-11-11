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

import { PoolLifecycleState, Sandbox } from "@alibaba-group/opensandbox";

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

test("readiness timeout deletes the failed sandbox and replenishes", async () => {
  const poolName = uniquePoolName("readiness-timeout");
  let failedSandboxId: string | undefined;
  let preparedSandboxId: string | undefined;
  let preparerCalls = 0;
  const pool = createPool(poolName, {
    warmupReadyTimeoutSeconds: 1,
    warmupHealthCheckPollingIntervalMillis: 100,
    warmupHealthCheck: async (sandbox) => {
      failedSandboxId ??= String(sandbox.id);
      return String(sandbox.id) !== failedSandboxId && await sandbox.isHealthy();
    },
    warmupSandboxPreparer: async (sandbox) => {
      preparerCalls += 1;
      preparedSandboxId = String(sandbox.id);
    },
  });

  try {
    await pool.start();
    await eventually("replacement after readiness timeout", async () => {
      const snapshot = await pool.snapshot();
      const ids = await taggedSandboxIds(poolName);
      return failedSandboxId !== undefined && preparedSandboxId !== undefined &&
        failedSandboxId !== preparedSandboxId && !ids.includes(failedSandboxId) &&
        ids.length === 1 && snapshot.idleCount === 1 && snapshot.inFlightOperations === 0;
    });
    expect(preparerCalls).toBe(1);
    expect((await pool.snapshotIdleEntries())[0]?.sandboxId).toBe(preparedSandboxId);
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("post-prepare timeout deletes the failed sandbox and prepares each replacement once", async () => {
  const poolName = uniquePoolName("post-prepare-timeout");
  let failedSandboxId: string | undefined;
  const prepares = new Map<string, number>();
  const pool = createPool(poolName, {
    warmupPostPrepareHealthCheckTimeoutSeconds: 1,
    warmupHealthCheckPollingIntervalMillis: 100,
    warmupSandboxPreparer: async (sandbox) => {
      const id = String(sandbox.id);
      prepares.set(id, (prepares.get(id) ?? 0) + 1);
    },
    warmupPostPrepareHealthCheck: async (sandbox) => {
      failedSandboxId ??= String(sandbox.id);
      return String(sandbox.id) !== failedSandboxId && await sandbox.isHealthy();
    },
  });

  try {
    await pool.start();
    await eventually("replacement after post-prepare timeout", async () => {
      const snapshot = await pool.snapshot();
      const ids = await taggedSandboxIds(poolName);
      return failedSandboxId !== undefined && !ids.includes(failedSandboxId) &&
        ids.length === 1 && snapshot.idleCount === 1 && snapshot.inFlightOperations === 0;
    });
    expect([...prepares.values()]).toEqual([1, 1]);
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("forced shutdown deletes a sandbox waiting in the warmup delay", async () => {
  const poolName = uniquePoolName("delayed-shutdown");
  let healthCheckCalls = 0;
  let preparerCalls = 0;
  const pool = createPool(poolName, {
    warmupHealthCheckInitialDelayMillis: 30_000,
    warmupHealthCheck: async () => { healthCheckCalls += 1; return true; },
    warmupSandboxPreparer: async () => { preparerCalls += 1; },
    drainTimeoutSeconds: 0.2,
  });

  try {
    await pool.start();
    await eventually("sandbox in warmup delay", async () => {
      const snapshot = await pool.snapshot();
      return (await taggedSandboxIds(poolName)).length === 1 &&
        snapshot.inFlightOperations === 1 && healthCheckCalls === 0;
    });
    const startedAt = Date.now();
    await pool.shutdown(false);
    expect(Date.now() - startedAt).toBeLessThan(10_000);
    const snapshot = await pool.snapshot();
    expect(snapshot.lifecycleState).toBe(PoolLifecycleState.STOPPED);
    expect(snapshot.idleCount).toBe(0);
    expect(healthCheckCalls).toBe(0);
    expect(preparerCalls).toBe(0);
    await eventually("delayed warmup cleanup", async () =>
      (await pool.snapshot()).inFlightOperations === 0 &&
      (await taggedSandboxIds(poolName)).length === 0,
    );
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);

test("warmupCreateQps admits real creation across fixed reconcile batches", async () => {
  const poolName = uniquePoolName("create-qps");
  const firstAdmission = deferred<number>();
  const secondAdmission = deferred<number>();
  let admissions = 0;
  const pool = createPool(poolName, {
    maxIdle: 3,
    warmupCreateQps: 1,
    warmupConcurrency: 3,
    warmupSkipHealthCheck: true,
    sandboxCreator: async (context) => {
      admissions += 1;
      const admittedAt = Date.now();
      if (admissions === 1) firstAdmission.resolve(admittedAt);
      if (admissions === 2) secondAdmission.resolve(admittedAt);
      return await Sandbox.create({
        ...context.creationSpec,
        connectionConfig: context.createConnectionConfig,
        timeoutSeconds: context.idleTimeoutSeconds,
        skipHealthCheck: true,
        signal: context.signal,
      });
    },
  });

  try {
    await pool.start();
    const firstAt = await firstAdmission.promise;
    expect(await Promise.race([
      secondAdmission.promise.then(() => true),
      sleep(400).then(() => false),
    ])).toBe(false);
    await eventually("QPS-limited warmup", async () => {
      const snapshot = await pool.snapshot();
      return admissions === 3 && snapshot.idleCount === 3 && snapshot.inFlightOperations === 0;
    });
    const secondAt = await secondAdmission.promise;
    expect(secondAt - firstAt).toBeGreaterThanOrEqual(500);
    expect(await pool.snapshotIdleEntries()).toHaveLength(3);
  } finally {
    await cleanupPool(poolName, [pool]);
  }
}, POOL_TEST_TIMEOUT);
