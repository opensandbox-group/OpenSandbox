import assert from "node:assert/strict";
import test from "node:test";

import {
  AcquirePolicy,
  ConnectionConfig,
  InMemoryPoolStateStore,
  PoolEmptyException,
  PoolLifecycleState,
  PooledSandboxCreateReason,
  SandboxPool,
} from "../dist/index.js";

async function eventually(check, timeoutMs = 2_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.fail("condition did not become true before timeout");
}

function createPoolFixture({ healthById = {}, renewFailureIds = new Set() } = {}) {
  const calls = [];
  const sandboxes = {
    async createSandbox(request) {
      calls.push({ method: "create", request });
      return { id: "direct-1" };
    },
    async getSandboxEndpoint(sandboxId, port) {
      calls.push({ method: "endpoint", sandboxId, port });
      return { endpoint: `${sandboxId}.internal:${port}`, headers: {} };
    },
    async renewSandboxExpiration(sandboxId, body) {
      calls.push({ method: "renew", sandboxId, body });
      if (renewFailureIds.has(sandboxId)) throw new Error("renew failed");
      return { expiresAt: new Date(body.expiresAt) };
    },
    async deleteSandbox(sandboxId) {
      calls.push({ method: "kill", sandboxId });
    },
  };
  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack({ execdBaseUrl }) {
      const sandboxId = new URL(execdBaseUrl).hostname.split(".")[0];
      return {
        commands: {},
        files: {},
        health: { async ping() { return healthById[sandboxId] ?? true; } },
        metrics: {},
      };
    },
    createEgressStack() {
      return { egress: {} };
    },
  };
  const connectionConfig = new ConnectionConfig({
    domain: "http://127.0.0.1:8080",
    disableMetrics: true,
  });
  let nextId = 0;
  const created = [];
  const sandboxCreator = async () => {
    const id = `warm-${++nextId}`;
    const sandbox = {
      id,
      async isHealthy() { return true; },
      async renew(timeoutSeconds) { calls.push({ method: "warmup-renew", id, timeoutSeconds }); },
      async kill() { calls.push({ method: "creator-kill", id }); },
      async close() { calls.push({ method: "creator-close", id }); },
    };
    created.push(sandbox);
    return sandbox;
  };

  return { adapterFactory, calls, connectionConfig, created, sandboxCreator, sandboxes };
}

test("InMemoryPoolStateStore atomically takes idle entries in FIFO order", async () => {
  const store = new InMemoryPoolStateStore();
  await store.setIdleEntryTtl("pool", 60);
  await store.putIdle("pool", "one");
  await store.putIdle("pool", "two");
  await store.putIdle("pool", "one");

  const [first, second] = await Promise.all([
    store.tryTakeIdle("pool"),
    store.tryTakeIdle("pool"),
  ]);
  assert.deepEqual([first, second], ["one", "two"]);
  assert.equal(await store.tryTakeIdle("pool"), undefined);
});

test("InMemoryPoolStateStore enforces TTL filtering and primary ownership", async () => {
  const store = new InMemoryPoolStateStore();
  await store.setIdleEntryTtl("pool", 10);
  await store.putIdle("pool", "near-expiry");

  const taken = await store.tryTakeIdleWithMinTtl("pool", 20);
  assert.equal(taken.sandboxId, undefined);
  assert.deepEqual(taken.discardedAliveSandboxIds, ["near-expiry"]);

  assert.equal(await store.tryAcquirePrimaryLock("pool", "owner-a", 60), true);
  assert.equal(await store.tryAcquirePrimaryLock("pool", "owner-b", 60), false);
  assert.equal(await store.renewPrimaryLock("pool", "owner-b", 60), false);
  assert.equal(await store.tryAcquirePrimaryLock("pool", "owner-b", 60), false);
  await store.releasePrimaryLock("pool", "owner-a");
  assert.equal(await store.tryAcquirePrimaryLock("pool", "owner-b", 60), true);
});

test("InMemoryPoolStateStore removes stale FIFO positions before an id is reused", async () => {
  const store = new InMemoryPoolStateStore();
  await store.putIdle("pool", "one");
  await store.putIdle("pool", "two");
  await store.removeIdle("pool", "one");
  await store.putIdle("pool", "one");

  assert.equal(await store.tryTakeIdle("pool"), "two");
  assert.equal(await store.tryTakeIdle("pool"), "one");
});

test("concurrent start calls wait for the same initialization", async () => {
  const fixture = createPoolFixture();
  const store = new InMemoryPoolStateStore();
  const setMaxIdle = store.setMaxIdle.bind(store);
  let setMaxIdleCalls = 0;
  let initializationStarted;
  const started = new Promise((resolve) => { initializationStarted = resolve; });
  let releaseInitialization;
  const gate = new Promise((resolve) => { releaseInitialization = resolve; });
  store.setMaxIdle = async (...args) => {
    setMaxIdleCalls += 1;
    initializationStarted();
    await gate;
    await setMaxIdle(...args);
  };
  const pool = SandboxPool.create({
    poolName: "concurrent-start-pool",
    maxIdle: 0,
    stateStore: store,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });

  const firstStart = pool.start();
  let secondStartDone = false;
  const secondStart = pool.start().then(() => { secondStartDone = true; });
  await started;
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(secondStartDone, false);

  releaseInitialization();
  await Promise.all([firstStart, secondStart]);
  assert.equal(setMaxIdleCalls, 1);
  assert.equal((await pool.snapshot()).lifecycleState, PoolLifecycleState.RUNNING);
  await pool.shutdown();
});

test("SandboxPool warms, acquires, renews, and replenishes an idle sandbox", async () => {
  const fixture = createPoolFixture();
  const pool = SandboxPool.create({
    poolName: "unit-pool",
    maxIdle: 2,
    warmupConcurrency: 2,
    reconcileIntervalSeconds: 60,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });

  await pool.start();
  await eventually(async () => (await pool.snapshot()).idleCount === 2);

  const sandbox = await pool.acquire({ sandboxTimeoutSeconds: 90 });
  assert.equal(sandbox.id, "warm-1");
  assert.ok(fixture.calls.some((call) => call.method === "renew" && call.sandboxId === "warm-1"));

  await eventually(async () => (await pool.snapshot()).idleCount === 2);
  assert.equal(fixture.created.length, 3);
  await sandbox.close();
  await pool.shutdown();
  assert.equal((await pool.snapshot()).lifecycleState, PoolLifecycleState.STOPPED);
});

test("SandboxPool does not close a caller-initialized connection transport", async () => {
  const fixture = createPoolFixture();
  const suppliedConfig = fixture.connectionConfig.withTransportIfMissing();
  const closeTransport = suppliedConfig.closeTransport.bind(suppliedConfig);
  let closeCalls = 0;
  suppliedConfig.closeTransport = async () => {
    closeCalls += 1;
    await closeTransport();
  };
  const pool = SandboxPool.create({
    poolName: "transport-ownership-pool",
    maxIdle: 1,
    connectionConfig: suppliedConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    await pool.shutdown();
    assert.equal(closeCalls, 0);
  } finally {
    await pool.shutdown(false).catch(() => undefined);
    await suppliedConfig.closeTransport();
  }
});

test("SandboxPool renews primary ownership while warmup creation is in flight", async () => {
  const fixture = createPoolFixture();
  const store = new InMemoryPoolStateStore();
  const renewPrimaryLock = store.renewPrimaryLock.bind(store);
  let renewCount = 0;
  store.renewPrimaryLock = async (...args) => {
    renewCount += 1;
    return renewPrimaryLock(...args);
  };
  let creatorStarted;
  const started = new Promise((resolve) => { creatorStarted = resolve; });
  let releaseCreator;
  const gate = new Promise((resolve) => { releaseCreator = resolve; });
  const pool = SandboxPool.create({
    poolName: "heartbeat-pool",
    maxIdle: 1,
    stateStore: store,
    primaryLockTtlSeconds: 0.3,
    reconcileIntervalSeconds: 60,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: async (context) => {
      creatorStarted();
      await gate;
      return fixture.sandboxCreator(context);
    },
  });

  await pool.start();
  await started;
  await eventually(() => renewCount >= 2);
  releaseCreator();
  await eventually(async () => (await pool.snapshot()).idleCount === 1);
  await pool.shutdown();
});

test("SandboxPool fail-fast acquire reports an empty pool", async () => {
  const fixture = createPoolFixture();
  const pool = SandboxPool.create({
    poolName: "empty-pool",
    maxIdle: 0,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });
  await pool.start();
  await assert.rejects(
    pool.acquire({ policy: AcquirePolicy.FAIL_FAST }),
    PoolEmptyException,
  );
  await pool.shutdown();
});

test("direct-create fallback uses the pool idle TTL before applying the acquired TTL", async () => {
  const fixture = createPoolFixture();
  const pool = SandboxPool.create({
    poolName: "direct-pool",
    maxIdle: 0,
    idleTimeoutSeconds: 120,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
  });
  await pool.start();

  const sandbox = await pool.acquire({ sandboxTimeoutSeconds: 45 });
  assert.equal(sandbox.id, "direct-1");
  assert.equal(fixture.calls.find((call) => call.method === "create").request.timeout, 120);
  assert.ok(fixture.calls.some((call) => call.method === "renew" && call.sandboxId === "direct-1"));
  await sandbox.close();
  await pool.shutdown();
});

test("custom creator receives the cross-language direct-create reason", async () => {
  const fixture = createPoolFixture();
  let createContext;
  const pool = SandboxPool.create({
    poolName: "creator-context-pool",
    maxIdle: 0,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: async (context) => {
      createContext = context;
      return fixture.sandboxCreator(context);
    },
  });
  await pool.start();

  const sandbox = await pool.acquire();
  assert.equal(createContext.reason, PooledSandboxCreateReason.DIRECT_CREATE);
  await sandbox.close();
  await pool.shutdown();
});

test("SandboxPool resize and releaseAllIdle update observable state", async () => {
  const fixture = createPoolFixture();
  const store = new InMemoryPoolStateStore();
  const pool = SandboxPool.create({
    poolName: "resize-pool",
    maxIdle: 1,
    stateStore: store,
    reconcileIntervalSeconds: 60,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });
  await pool.start();
  await eventually(async () => (await pool.snapshot()).idleCount === 1);
  await pool.resize(2);
  await eventually(async () => (await pool.snapshot()).idleCount === 2);
  await store.setMaxIdle("resize-pool", 0);
  assert.equal(await pool.releaseAllIdle(), 2);
  assert.equal((await pool.snapshot()).idleCount, 0);
  await pool.shutdown(false);
});

test("retry-next-idle skips an unhealthy sandbox without duplicating acquisition", async () => {
  const fixture = createPoolFixture({ healthById: { bad: false, good: true } });
  const store = new InMemoryPoolStateStore();
  const pool = SandboxPool.create({
    poolName: "retry-pool",
    maxIdle: 0,
    stateStore: store,
    maxAcquireRetries: 2,
    acquireReadyTimeoutSeconds: 0.01,
    acquireHealthCheckPollingIntervalMillis: 1,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });
  await pool.start();
  await store.putIdle("retry-pool", "bad");
  await store.putIdle("retry-pool", "good");

  const sandbox = await pool.acquire({ policy: AcquirePolicy.RETRY_NEXT_IDLE });
  assert.equal(sandbox.id, "good");
  await eventually(async () => fixture.calls.some((call) => call.method === "kill" && call.sandboxId === "bad"));
  assert.equal(await store.tryTakeIdle("retry-pool"), undefined);
  await sandbox.close();
  await pool.shutdown();
});

test("renew failure is terminal and does not consume another idle sandbox", async () => {
  const fixture = createPoolFixture({ renewFailureIds: new Set(["first"]) });
  const store = new InMemoryPoolStateStore();
  const tryTakeIdleWithMinTtl = store.tryTakeIdleWithMinTtl.bind(store);
  const acquiredIds = [];
  store.tryTakeIdleWithMinTtl = async (...args) => {
    const result = await tryTakeIdleWithMinTtl(...args);
    acquiredIds.push(result.sandboxId);
    return result;
  };
  const pool = SandboxPool.create({
    poolName: "renew-pool",
    maxIdle: 0,
    stateStore: store,
    maxAcquireRetries: 2,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });
  await pool.start();
  await store.putIdle("renew-pool", "first");
  await store.putIdle("renew-pool", "second");

  await assert.rejects(
    pool.acquire({
      sandboxTimeoutSeconds: 60,
      policy: AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE,
    }),
    /renew failed/,
  );
  assert.deepEqual(acquiredIds, ["first"]);
  await pool.shutdown();
});

test("forced shutdown does not wait for a creator that ignores cancellation", async () => {
  const fixture = createPoolFixture();
  let creatorStarted;
  const started = new Promise((resolve) => { creatorStarted = resolve; });
  let releaseCreator;
  const creatorGate = new Promise((resolve) => { releaseCreator = resolve; });
  const pool = SandboxPool.create({
    poolName: "forced-shutdown-pool",
    maxIdle: 1,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: async () => {
      creatorStarted();
      await creatorGate;
      return fixture.sandboxCreator();
    },
  });

  await pool.start();
  await started;
  await pool.shutdown(false);
  assert.equal((await pool.snapshot()).lifecycleState, PoolLifecycleState.STOPPED);

  releaseCreator();
  await eventually(async () => fixture.calls.some((call) => call.method === "creator-kill"));
});

test("acquire disposes a sandbox if forced shutdown retires the pool run", async () => {
  const fixture = createPoolFixture();
  const store = new InMemoryPoolStateStore();
  const getSandboxEndpoint = fixture.sandboxes.getSandboxEndpoint.bind(fixture.sandboxes);
  let connectStarted;
  const started = new Promise((resolve) => { connectStarted = resolve; });
  let releaseConnect;
  const gate = new Promise((resolve) => { releaseConnect = resolve; });
  fixture.sandboxes.getSandboxEndpoint = async (...args) => {
    connectStarted();
    await gate;
    return getSandboxEndpoint(...args);
  };
  const pool = SandboxPool.create({
    poolName: "shutdown-acquire-pool",
    maxIdle: 0,
    stateStore: store,
    connectionConfig: fixture.connectionConfig,
    creationSpec: { image: "ubuntu", adapterFactory: fixture.adapterFactory },
    sandboxCreator: fixture.sandboxCreator,
  });
  await pool.start();
  await store.putIdle("shutdown-acquire-pool", "idle");

  const acquire = pool.acquire({ policy: AcquirePolicy.FAIL_FAST });
  await started;
  await pool.shutdown(false);
  releaseConnect();

  await assert.rejects(acquire, /is not running/);
  await eventually(async () => fixture.calls.some((call) => call.method === "kill" && call.sandboxId === "idle"));
});
