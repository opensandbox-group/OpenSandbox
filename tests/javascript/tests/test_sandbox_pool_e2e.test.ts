// Copyright 2026 Alibaba Group Holding Ltd.
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
  PoolEmptyException,
  Sandbox,
  SandboxPool,
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
    reconcileIntervalSeconds: 1,
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
