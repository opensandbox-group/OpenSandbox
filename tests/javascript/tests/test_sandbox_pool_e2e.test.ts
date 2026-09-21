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
  PoolDestroyedException,
  PoolEmptyException,
  PoolLifecycleState,
  Sandbox,
  SandboxPool,
  SandboxPoolManager,
} from "@alibaba-group/opensandbox";

import { createConnectionConfig, getSandboxImage } from "./base_e2e.ts";

async function eventually(check: () => Promise<boolean>, timeoutMs = 120_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error("pool did not converge before timeout");
}

test("client pool warms, acquires, drains, and falls back to direct create", async () => {
  const poolName = `js-pool-${Math.random().toString(16).slice(2, 10)}`;
  const pool = SandboxPool.create({
    poolName,
    maxIdle: 1,
    stateStore: new InMemoryPoolStateStore(),
    connectionConfig: createConnectionConfig(),
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    warmupReadyTimeoutSeconds: 60,
    acquireReadyTimeoutSeconds: 60,
  });
  const acquired: Sandbox[] = [];

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);

    const warm = await pool.acquire({
      policy: AcquirePolicy.FAIL_FAST,
      sandboxTimeoutSeconds: 5 * 60,
    });
    acquired.push(warm);
    const result = await warm.commands.run("echo js-pool-ok");
    expect(result.error).toBeUndefined();
    expect(result.logs.stdout[0]?.text).toBe("js-pool-ok");

    await pool.resize(0);
    await pool.releaseAllIdle();
    await expect(pool.acquire({ policy: AcquirePolicy.FAIL_FAST })).rejects.toBeInstanceOf(
      PoolEmptyException,
    );

    const direct = await pool.acquire({
      policy: AcquirePolicy.DIRECT_CREATE,
      sandboxTimeoutSeconds: 5 * 60,
    });
    acquired.push(direct);
    expect(await direct.isHealthy()).toBe(true);
  } finally {
    await pool.resize(0).catch(() => undefined);
    await pool.releaseAllIdle().catch(() => undefined);
    await pool.shutdown(false).catch(() => undefined);
    for (const sandbox of acquired) {
      await sandbox.kill().catch(() => undefined);
      await sandbox.close().catch(() => undefined);
    }
  }
}, 5 * 60_000);

test("client pool runs staged warmup before publishing idle", async () => {
  const poolName = `js-pool-staged-${Math.random().toString(16).slice(2, 10)}`;
  const marker = `/tmp/${poolName}.ready`;
  const events: string[] = [];
  let postPrepareCalls = 0;
  const pool = SandboxPool.create({
    poolName,
    maxIdle: 1,
    stateStore: new InMemoryPoolStateStore(),
    connectionConfig: createConnectionConfig(),
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    warmupReadyTimeoutSeconds: 1,
    warmupHealthCheckInitialDelayMillis: 2_000,
    warmupHealthCheck: async () => {
      events.push("readiness");
      return true;
    },
    warmupSandboxPreparer: async (sandbox) => {
      events.push("prepare");
      const result = await sandbox.commands.run(`printf prepared > ${marker}`);
      if (result.error) throw new Error(String(result.error));
    },
    warmupPostPrepareHealthCheck: async (sandbox) => {
      events.push("post-prepare-readiness");
      postPrepareCalls += 1;
      if (postPrepareCalls === 1) return false;
      const result = await sandbox.commands.run(`test -f ${marker}`);
      return result.error === undefined;
    },
  });
  let acquired: Sandbox | undefined;

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    acquired = await pool.acquire({ policy: AcquirePolicy.FAIL_FAST, sandboxTimeoutSeconds: 5 * 60 });
    const result = await acquired.commands.run(`cat ${marker}`);
    expect(result.error).toBeUndefined();
    expect(result.logs.stdout[0]?.text).toBe("prepared");
    expect(events).toEqual([
      "readiness",
      "prepare",
      "post-prepare-readiness",
      "post-prepare-readiness",
    ]);
  } finally {
    await pool.resize(0).catch(() => undefined);
    await pool.releaseAllIdle().catch(() => undefined);
    await pool.shutdown(false).catch(() => undefined);
    await acquired?.kill().catch(() => undefined);
    await acquired?.close().catch(() => undefined);
  }
}, 5 * 60_000);

test("graceful shutdown drains an admitted staged warmup", async () => {
  const poolName = `js-pool-drain-${Math.random().toString(16).slice(2, 10)}`;
  let preparerStartedResolve: (() => void) | undefined;
  const preparerStarted = new Promise<void>((resolve) => { preparerStartedResolve = resolve; });
  let releasePreparer: (() => void) | undefined;
  const preparerGate = new Promise<void>((resolve) => { releasePreparer = resolve; });
  const pool = SandboxPool.create({
    poolName,
    maxIdle: 1,
    stateStore: new InMemoryPoolStateStore(),
    connectionConfig: createConnectionConfig(),
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    drainTimeoutSeconds: 60,
    warmupReadyTimeoutSeconds: 60,
    warmupSandboxPreparer: async () => {
      preparerStartedResolve?.();
      await preparerGate;
    },
  });

  try {
    await pool.start();
    await preparerStarted;
    const shutdown = pool.shutdown(true);
    let completed = false;
    void shutdown.then(() => { completed = true; });
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(completed).toBe(false);
    releasePreparer?.();
    await shutdown;
    const snapshot = await pool.snapshot();
    expect(snapshot.lifecycleState).toBe(PoolLifecycleState.STOPPED);
    expect(snapshot.idleCount).toBe(1);
  } finally {
    releasePreparer?.();
    await pool.releaseAllIdle().catch(() => undefined);
    await pool.shutdown(false).catch(() => undefined);
  }
}, 5 * 60_000);

test("pool manager destroy drains and fences a live pool namespace", async () => {
  const poolName = `js-pool-destroy-${Math.random().toString(16).slice(2, 10)}`;
  const stateStore = new InMemoryPoolStateStore();
  const connectionConfig = createConnectionConfig();
  const pool = SandboxPool.create({
    poolName,
    maxIdle: 1,
    stateStore,
    connectionConfig,
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    warmupReadyTimeoutSeconds: 60,
  });
  const manager = new SandboxPoolManager({ stateStore, connectionConfig });

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    const result = await manager.destroy(poolName);
    expect(result.drainedIdleCount).toBe(1);
    expect(result.killedIdleCount).toBe(1);
    await expect(pool.acquire()).rejects.toBeInstanceOf(PoolDestroyedException);
    await eventually(async () => (await pool.snapshot()).lifecycleState === PoolLifecycleState.STOPPED);
    await expect(pool.start()).rejects.toBeInstanceOf(PoolDestroyedException);
  } finally {
    await pool.shutdown(false).catch(() => undefined);
  }
}, 5 * 60_000);
