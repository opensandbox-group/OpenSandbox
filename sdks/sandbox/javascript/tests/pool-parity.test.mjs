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

import assert from "node:assert/strict";
import test from "node:test";

import {
  ConnectionConfig,
  InMemoryPoolStateStore,
  PoolDestroyState,
  PoolDestroyIncompleteException,
  PoolDestroyedException,
  PoolLifecycleState,
  PoolStateStoreUnavailableException,
  SandboxPool,
  SandboxPoolManager,
} from "../dist/index.js";

async function eventually(check, timeoutMs = 3_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.fail("condition did not become true before timeout");
}

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function fakeSandbox(id, events = []) {
  return {
    id,
    async isHealthy() { events.push(`${id}:healthy`); return true; },
    async renew() { events.push(`${id}:renew`); },
    async kill() { events.push(`${id}:kill`); },
    async close() { events.push(`${id}:close`); },
  };
}

function poolOptions(overrides = {}) {
  return {
    poolName: `parity-${Math.random().toString(16).slice(2)}`,
    maxIdle: 1,
    connectionConfig: { domain: "localhost:8080", disableMetrics: true },
    creationSpec: { image: "test" },
    warmupSkipHealthCheck: true,
    ...overrides,
  };
}

test("warmup admission uses fixed one-second QPS batches and does not wait for create", async () => {
  const createGate = deferred();
  let createCount = 0;
  const pool = SandboxPool.create(poolOptions({
    maxIdle: 12,
    warmupCreateQps: 10,
    warmupConcurrency: 1,
    sandboxCreator: async () => {
      const id = `qps-${++createCount}`;
      await createGate.promise;
      return fakeSandbox(id);
    },
  }));

  try {
    await pool.start();
    await eventually(() => createCount === 10, 500);
    await new Promise((resolve) => setTimeout(resolve, 100));
    assert.equal(createCount, 10);
    await eventually(() => createCount === 12, 1_500);
  } finally {
    createGate.resolve();
    await pool.shutdown(false);
  }
});

test("warmup create concurrency uses Kotlin-compatible 1.5x QPS headroom", async () => {
  const createGate = deferred();
  let started = 0;
  let active = 0;
  let maxActive = 0;
  const pool = SandboxPool.create(poolOptions({
    maxIdle: 20,
    warmupCreateQps: 10,
    sandboxCreator: async () => {
      started += 1;
      active += 1;
      maxActive = Math.max(maxActive, active);
      await createGate.promise;
      active -= 1;
      return fakeSandbox(`create-${started}`);
    },
  }));

  try {
    await pool.start();
    await eventually(() => started === 15, 1_500);
    await new Promise((resolve) => setTimeout(resolve, 50));
    assert.equal(maxActive, 15);
    assert.equal(started, 15);
  } finally {
    createGate.resolve();
    await pool.shutdown(false);
  }
});

test("warmupConcurrency bounds post-create stages", async () => {
  const healthGate = deferred();
  let nextId = 0;
  let active = 0;
  let maxActive = 0;
  const pool = SandboxPool.create(poolOptions({
    maxIdle: 4,
    warmupCreateQps: 4,
    warmupConcurrency: 2,
    warmupSkipHealthCheck: false,
    sandboxCreator: async () => fakeSandbox(`concurrency-${++nextId}`),
    warmupHealthCheck: async () => {
      active += 1;
      maxActive = Math.max(maxActive, active);
      await healthGate.promise;
      active -= 1;
      return true;
    },
  }));

  try {
    await pool.start();
    await eventually(() => active === 2);
    assert.equal(maxActive, 2);
    healthGate.resolve();
    await eventually(async () => (await pool.snapshot()).idleCount === 4);
    assert.equal(maxActive, 2);
  } finally {
    healthGate.resolve();
    await pool.shutdown(false);
  }
});

test("warmupConcurrency permit is released between readiness polling attempts", async () => {
  let nextId = 0;
  const attempts = new Map();
  const checkOrder = [];
  const pool = SandboxPool.create(poolOptions({
    maxIdle: 2,
    warmupCreateQps: 2,
    warmupConcurrency: 1,
    warmupSkipHealthCheck: false,
    warmupHealthCheckPollingIntervalMillis: 100,
    sandboxCreator: async () => fakeSandbox(`polling-${++nextId}`),
    warmupHealthCheck: async (sandbox) => {
      checkOrder.push(sandbox.id);
      const attempt = (attempts.get(sandbox.id) ?? 0) + 1;
      attempts.set(sandbox.id, attempt);
      return attempt >= 2;
    },
  }));

  try {
    await pool.start();
    await eventually(() => checkOrder.length >= 2);
    assert.notEqual(checkOrder[0], checkOrder[1]);
    await eventually(async () => (await pool.snapshot()).idleCount === 2);
  } finally {
    await pool.shutdown(false);
  }
});

test("warmup initial delay is capped by the readiness deadline with one final attempt", async () => {
  let healthChecks = 0;
  const startedAt = Date.now();
  const pool = SandboxPool.create(poolOptions({
    warmupSkipHealthCheck: false,
    warmupReadyTimeoutSeconds: 0.02,
    warmupHealthCheckInitialDelayMillis: 1_000,
    sandboxCreator: async () => fakeSandbox("final-attempt"),
    warmupHealthCheck: async () => { healthChecks += 1; return true; },
  }));

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    assert.equal(healthChecks, 1);
    assert.ok(Date.now() - startedAt < 500);
  } finally {
    await pool.shutdown(false);
  }
});

test("capped warmup initial delay makes its final attempt even when the delay timer resolves early", async (t) => {
  // Timers can fire before performance.now() crosses the readiness deadline.
  // Skew the clock so the capped delay ends with budget left, then let the check push it past the deadline.
  const realNow = performance.now.bind(performance);
  let skewMillis = 0;
  t.mock.method(performance, "now", () => realNow() - skewMillis);
  let creates = 0;
  let healthChecks = 0;
  const pool = SandboxPool.create(poolOptions({
    warmupSkipHealthCheck: false,
    warmupReadyTimeoutSeconds: 0.05,
    warmupHealthCheckInitialDelayMillis: 1_000,
    sandboxCreator: async () => {
      creates += 1;
      setTimeout(() => { skewMillis = 25; }, 10);
      return fakeSandbox(`early-timer-${creates}`);
    },
    warmupHealthCheck: async () => {
      healthChecks += 1;
      skewMillis = 0;
      return true;
    },
  }));

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1, 500);
    assert.equal(creates, 1);
    assert.equal(healthChecks, 1);
  } finally {
    await pool.shutdown(false);
  }
});

test("staged warmup runs readiness, preparer, post-check, renew, and commit in order", async () => {
  const events = [];
  let postAttempts = 0;
  const pool = SandboxPool.create(poolOptions({
    sandboxCreator: async () => {
      events.push("create");
      return fakeSandbox("staged", events);
    },
    warmupSkipHealthCheck: false,
    warmupHealthCheck: async () => { events.push("readiness"); return true; },
    warmupSandboxPreparer: async () => { events.push("prepare"); },
    warmupPostPrepareHealthCheck: async () => {
      events.push("post-check");
      postAttempts += 1;
      return postAttempts >= 2;
    },
    warmupHealthCheckPollingIntervalMillis: 1,
  }));

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    assert.deepEqual(events.slice(0, 6), [
      "create",
      "readiness",
      "prepare",
      "post-check",
      "post-check",
      "staged:renew",
    ]);
    assert.equal(events.filter((event) => event === "prepare").length, 1);
  } finally {
    await pool.shutdown(false);
  }
});

test("restart does not let an old warmup block or publish into the new run", async () => {
  const oldGate = deferred();
  const events = [];
  let createCount = 0;
  const pool = SandboxPool.create(poolOptions({
    sandboxCreator: async () => {
      const id = `run-${++createCount}`;
      if (id === "run-1") await oldGate.promise;
      return fakeSandbox(id, events);
    },
  }));

  try {
    await pool.start();
    await eventually(() => createCount === 1);
    await pool.shutdown(false);
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    assert.equal(createCount, 2);
    oldGate.resolve();
    await eventually(() => events.includes("run-1:kill"));
    assert.deepEqual((await pool.snapshotIdleEntries()).map((entry) => entry.sandboxId), ["run-2"]);
  } finally {
    oldGate.resolve();
    await pool.shutdown(false);
  }
});

test("SandboxPoolManager drains idle sandboxes and fences the namespace", async () => {
  const store = new InMemoryPoolStateStore();
  await store.setIdleEntryTtl("destroy-pool", 60);
  await store.putIdle("destroy-pool", "one");
  await store.putIdle("destroy-pool", "two");
  const killed = [];
  const adapterFactory = {
    createLifecycleStack() {
      return {
        sandboxes: {
          async deleteSandbox(id) { killed.push(id); },
        },
      };
    },
  };
  const manager = new SandboxPoolManager({
    stateStore: store,
    connectionConfig: new ConnectionConfig({ domain: "localhost:8080" }),
    adapterFactory,
  });

  const result = await manager.destroy("destroy-pool");
  assert.equal(result.state, PoolDestroyState.DESTROYED);
  assert.equal(result.drainedIdleCount, 2);
  assert.equal(result.killedIdleCount, 2);
  assert.deepEqual(killed, ["one", "two"]);
  assert.equal(await store.getDestroyState("destroy-pool"), PoolDestroyState.DESTROYED);
  await assert.rejects(store.putIdle("destroy-pool", "three"), PoolDestroyedException);

  const pool = SandboxPool.create(poolOptions({
    poolName: "destroy-pool",
    stateStore: store,
    sandboxCreator: async () => fakeSandbox("never-created"),
  }));
  await assert.rejects(pool.start(), PoolDestroyedException);
  assert.equal((await pool.snapshot()).lifecycleState, PoolLifecycleState.STOPPED);
});

test("SandboxPoolManager bounds an in-flight kill and supports destroy retry", async () => {
  const store = new InMemoryPoolStateStore();
  await store.setIdleEntryTtl("timed-destroy", 60);
  await store.putIdle("timed-destroy", "slow");
  let killSignal;
  const adapterFactory = {
    createLifecycleStack() {
      return {
        sandboxes: {
          deleteSandbox(_id, signal) {
            killSignal = signal;
            // A custom adapter may ignore AbortSignal. destroy() must still
            // return at its own deadline rather than await this forever.
            return new Promise(() => {});
          },
        },
      };
    },
  };
  const manager = new SandboxPoolManager({
    stateStore: store,
    connectionConfig: new ConnectionConfig({ domain: "localhost:8080" }),
    adapterFactory,
  });

  const started = Date.now();
  await assert.rejects(
    manager.destroy("timed-destroy", { drainTimeoutSeconds: 0.02 }),
    PoolDestroyIncompleteException,
  );
  assert.ok(Date.now() - started < 500);
  assert.equal(killSignal?.aborted, true);
  assert.equal(
    await store.getDestroyState("timed-destroy"),
    PoolDestroyState.DESTROYING,
  );

  const retried = await manager.destroy("timed-destroy");
  assert.equal(retried.state, PoolDestroyState.DESTROYED);
});

test("in-memory maxIdle remains local to each pool instance", async () => {
  const store = new InMemoryPoolStateStore();
  await store.setMaxIdle("shared", 99);
  assert.equal(await store.getMaxIdle("shared"), undefined);
});

test("direct-create policy stays available during state-store outage", async () => {
  const store = new InMemoryPoolStateStore();
  const pool = SandboxPool.create(poolOptions({
    maxIdle: 0,
    stateStore: store,
    sandboxCreator: async () => fakeSandbox("outage-direct"),
  }));
  await pool.start();
  store.getDestroyState = async () => {
    throw new PoolStateStoreUnavailableException("getDestroyState");
  };
  store.tryTakeIdleWithMinTtl = async () => {
    throw new PoolStateStoreUnavailableException("tryTakeIdle");
  };

  try {
    const sandbox = await pool.acquire();
    assert.equal(sandbox.id, "outage-direct");
    await sandbox.close();
  } finally {
    await pool.shutdown(false);
  }
});

test("destroy tombstone remains visible after the local pool observes it and stops", async () => {
  const store = new InMemoryPoolStateStore();
  const pool = SandboxPool.create(poolOptions({
    poolName: "observed-destroy",
    maxIdle: 0,
    stateStore: store,
    sandboxCreator: async () => fakeSandbox("never-created"),
  }));
  await pool.start();
  await store.beginDestroy("observed-destroy", "manager");
  await eventually(async () => (await pool.snapshot()).lifecycleState === PoolLifecycleState.STOPPED, 1_500);
  await assert.rejects(pool.acquire(), PoolDestroyedException);
  await assert.rejects(pool.resize(1), PoolDestroyedException);
});
