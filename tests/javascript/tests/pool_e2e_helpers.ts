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
  InMemoryPoolStateStore,
  SandboxManager,
  SandboxPool,
  type PoolStateStore,
  type SandboxPoolOptions,
} from "@alibaba-group/opensandbox";

import { createConnectionConfig, getSandboxImage } from "./base_e2e.ts";

export const POOL_TEST_TIMEOUT = 5 * 60_000;

export function uniquePoolName(scenario: string): string {
  return `js-pool-${scenario}-${Math.random().toString(16).slice(2, 10)}`;
}

export function poolOptions(
  poolName: string,
  overrides: Partial<SandboxPoolOptions> = {},
): SandboxPoolOptions {
  return {
    poolName,
    ownerId: `${poolName}-owner`,
    maxIdle: 1,
    warmupConcurrency: 1,
    connectionConfig: createConnectionConfig(),
    stateStore: new InMemoryPoolStateStore(),
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName, suite: "sandbox-pool-javascript-e2e" },
      env: {
        E2E_TEST: "true",
        EXECD_API_GRACE_SHUTDOWN: "3s",
        EXECD_JUPYTER_IDLE_POLL_INTERVAL: "200ms",
      },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    acquireReadyTimeoutSeconds: 30,
    acquireHealthCheckPollingIntervalMillis: 50,
    warmupReadyTimeoutSeconds: 2 * 60,
    warmupHealthCheckPollingIntervalMillis: 100,
    drainTimeoutSeconds: 2,
    ...overrides,
  };
}

export function createPool(
  poolName: string,
  overrides: Partial<SandboxPoolOptions> = {},
): SandboxPool {
  return SandboxPool.create(poolOptions(poolName, overrides));
}

export async function eventually(
  description: string,
  check: () => boolean | Promise<boolean>,
  timeoutMs = 120_000,
  intervalMs = 250,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastError: unknown;
  while (Date.now() < deadline) {
    try {
      if (await check()) return;
    } catch (error) {
      lastError = error;
    }
    await sleep(intervalMs);
  }
  const suffix = lastError instanceof Error ? `: ${lastError.message}` : "";
  throw new Error(`Timed out waiting for ${description}${suffix}`);
}

export function deferred<T = void>(): {
  promise: Promise<T>;
  resolve: (value: T | PromiseLike<T>) => void;
  reject: (reason?: unknown) => void;
} {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

export function sleep(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

export async function taggedSandboxIds(poolName: string): Promise<string[]> {
  const manager = SandboxManager.create({ connectionConfig: createConnectionConfig() });
  try {
    const response = await manager.listSandboxInfos({
      metadata: { tag: poolName },
      pageSize: 100,
    });
    return response.items.map((sandbox) => sandbox.id);
  } finally {
    await manager.close();
  }
}

export async function cleanupPool(
  poolName: string,
  pools: Array<SandboxPool | undefined>,
): Promise<void> {
  for (const pool of pools) {
    if (!pool) continue;
    await pool.resize(0).catch(() => undefined);
    await pool.releaseAllIdle(8).catch(() => undefined);
    await pool.shutdown(false).catch(() => undefined);
  }
  const manager = SandboxManager.create({ connectionConfig: createConnectionConfig() });
  try {
    const response = await manager.listSandboxInfos({
      metadata: { tag: poolName },
      pageSize: 100,
    });
    await Promise.all(response.items.map((sandbox) => manager.killSandbox(sandbox.id).catch(() => undefined)));
  } finally {
    await manager.close();
  }
}

export function overrideStore(
  delegate: PoolStateStore,
  overrides: Partial<PoolStateStore>,
): PoolStateStore {
  return new Proxy(delegate, {
    get(target, property, receiver) {
      const override = Reflect.get(overrides, property, overrides);
      if (override !== undefined) {
        return typeof override === "function" ? override.bind(overrides) : override;
      }
      const value = Reflect.get(target, property, receiver);
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
}
