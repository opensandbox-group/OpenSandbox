import assert from "node:assert/strict";
import test from "node:test";

import { ConnectionConfig, Sandbox } from "../dist/index.js";

function createPendingConnectionConfig() {
  const calls = [];
  let notifyStarted;
  const started = new Promise((resolve) => {
    notifyStarted = resolve;
  });
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (input, init) => {
    const signal =
      init?.signal ??
      (typeof Request !== "undefined" && input instanceof Request
        ? input.signal
        : undefined);
    calls.push({ input, init, signal });
    notifyStarted();

    return await new Promise((_, reject) => {
      if (!signal) {
        reject(new Error("Expected request signal"));
        return;
      }
      const onAbort = () => reject(signal.reason);
      if (signal.aborted) onAbort();
      else signal.addEventListener("abort", onAbort, { once: true });
    });
  };

  let connectionConfig;
  try {
    connectionConfig = new ConnectionConfig({
      domain: "http://127.0.0.1:8080",
      disableMetrics: true,
    }).withTransportIfMissing();
  } finally {
    globalThis.fetch = originalFetch;
  }

  return { calls, connectionConfig, started };
}

function assertAbortError(err) {
  assert.equal(err?.name, "AbortError");
  return true;
}

test("Sandbox.create aborts the in-flight Lifecycle API request", async () => {
  const { calls, connectionConfig, started } = createPendingConnectionConfig();
  const controller = new AbortController();

  const creating = Sandbox.create({
    connectionConfig,
    image: "python:3.12",
    skipHealthCheck: true,
    signal: controller.signal,
  });

  await started;
  controller.abort();

  await assert.rejects(creating, assertAbortError);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].signal.aborted, true);
});

test("Sandbox.connect aborts the in-flight endpoint request", async () => {
  const { calls, connectionConfig, started } = createPendingConnectionConfig();
  const controller = new AbortController();

  const connecting = Sandbox.connect({
    connectionConfig,
    sandboxId: "sandbox-test-id",
    skipHealthCheck: true,
    signal: controller.signal,
  });

  await started;
  controller.abort();

  await assert.rejects(connecting, assertAbortError);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].signal.aborted, true);
});

test("Sandbox.create rejects a pre-aborted signal before issuing a request", async () => {
  const { calls, connectionConfig } = createPendingConnectionConfig();
  const controller = new AbortController();
  controller.abort();

  await assert.rejects(
    Sandbox.create({
      connectionConfig,
      image: "python:3.12",
      skipHealthCheck: true,
      signal: controller.signal,
    }),
    assertAbortError,
  );
  assert.equal(calls.length, 0);
});

test("Sandbox.create aborts a readiness check and cleans up the sandbox", async () => {
  const controller = new AbortController();
  let healthSignal;
  let notifyHealthStarted;
  const healthStarted = new Promise((resolve) => {
    notifyHealthStarted = resolve;
  });
  const deleted = [];
  let finishDelete;
  const deleteFinished = new Promise((resolve) => {
    finishDelete = resolve;
  });

  const sandboxes = {
    async createSandbox() {
      return { id: "sandbox-test-id" };
    },
    async getSandboxEndpoint(_sandboxId, port) {
      return { endpoint: `127.0.0.1:${port}`, headers: {} };
    },
    async deleteSandbox(sandboxId) {
      deleted.push(sandboxId);
      await deleteFinished;
    },
  };
  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack() {
      return {
        commands: {},
        files: {},
        health: {
          async ping(signal) {
            healthSignal = signal;
            notifyHealthStarted();
            return await new Promise((_, reject) => {
              const onAbort = () => reject(signal.reason);
              if (signal.aborted) onAbort();
              else signal.addEventListener("abort", onAbort, { once: true });
            });
          },
        },
        metrics: {},
      };
    },
    createEgressStack() {
      return { egress: {} };
    },
  };

  const creating = Sandbox.create({
    adapterFactory,
    connectionConfig: {
      domain: "http://127.0.0.1:8080",
      disableMetrics: true,
    },
    image: "python:3.12",
    signal: controller.signal,
  });

  await healthStarted;
  controller.abort();

  let timeout;
  const rejectionDeadline = new Promise((_, reject) => {
    timeout = setTimeout(
      () => reject(new Error("Cancellation did not reject promptly")),
      250,
    );
  });
  try {
    await assert.rejects(
      Promise.race([creating, rejectionDeadline]),
      assertAbortError,
    );
  } finally {
    clearTimeout(timeout);
  }
  assert.equal(healthSignal.aborted, true);
  assert.equal(healthSignal.reason, controller.signal.reason);
  assert.deepEqual(deleted, ["sandbox-test-id"]);
  finishDelete();
});
