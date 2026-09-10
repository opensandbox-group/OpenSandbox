import assert from "node:assert/strict";
import test from "node:test";
import { setTimeout as schedule } from "node:timers";
import { Sandbox, SandboxApiException, SandboxError, SandboxReadyTimeoutException } from "../dist/index.js";

const unavailable = (code = "KUBERNETES::POD_IP_NOT_AVAILABLE", statusCode = 404) =>
  new SandboxApiException({ message: "starting", statusCode, error: new SandboxError(code) });

function options(resolve, extra = {}) {
  return {
    sandboxId: "sb", readyTimeoutSeconds: 0.1, healthCheckPollingInterval: 1,
    connectionConfig: { domain: "localhost:8080" },
    adapterFactory: {
      createLifecycleStack: () => ({ sandboxes: { getSandboxEndpoint: resolve, resumeSandbox: async () => {} } }),
      createExecdStack: ({ endpointHeaders }) => ({ health: { ping: async () => endpointHeaders.token === "new" } }),
      createEgressStack: () => ({}),
    },
    ...extra,
  };
}

for (const result of [true, false, new Error("late custom failure")]) {
  test(`late custom result ${String(result)} is rejected after the callback finishes`, async (t) => {
    let now = 0;
    t.mock.method(performance, "now", () => now);
    for (const phase of ["health", "endpoint"]) {
      let calls = 0;
      const block = () => {
        calls++;
        now += 100;
        if (result instanceof Error) throw result;
        return result;
      };
      const opts = options(async () => {
        if (phase === "endpoint") block();
        return { endpoint: "localhost:44772", headers: {} };
      }, {
        readyTimeoutSeconds: 0.05,
        healthCheck: block,
      });
      await assert.rejects(Sandbox.connect(opts), SandboxReadyTimeoutException);
      assert.equal(calls, 1);
    }
  });
}

test("connect/resume retry only unresolved endpoint and keep fresh headers", async () => {
  for (const resume of [false, true]) {
    const calls = [];
    let failures = 2;
    const opts = options(async (_, port) => {
      calls.push(port);
      if (port === 44772 && failures-- > 0) throw unavailable();
      return { endpoint: "localhost:44772", headers: { token: "new" } };
    });
    const sb = await (resume ? Sandbox.resume(opts) : Sandbox.connect(opts));
    assert.equal(calls.length, 4);
    assert.equal(calls.filter(port => port !== 44772).length, 1);
    await sb.close();
  }
});

test("ordinary endpoint errors are returned unchanged without retry", async () => {
  for (const error of [unavailable("SANDBOX_NOT_FOUND"), unavailable(undefined, 401), unavailable(undefined, 403)]) {
    let calls = 0;
    await assert.rejects(Sandbox.connect(options(async () => { calls++; throw error; })), e => e === error);
    assert.equal(calls, 1);
  }
});

test("skip health still waits for endpoints and timeout retains last error", async () => {
  const error = unavailable();
  await assert.rejects(Sandbox.connect(options(async () => { throw error; }, { skipHealthCheck: true, readyTimeoutSeconds: 0.015 })),
    e => e instanceof SandboxReadyTimeoutException && e.cause === error);
});

test("cancellation stops polling with original reason", async () => {
  const controller = new AbortController();
  const reason = new Error("caller stopped");
  let calls = 0;
  const connecting = Sandbox.connect(options(async () => { calls++; throw unavailable(); }, { signal: controller.signal, healthCheckPollingInterval: 1000 }));
  setTimeout(() => controller.abort(reason), 10);
  await assert.rejects(connecting, e => e === reason);
  assert.equal(calls, 1);
});

test("endpoint and health requests share the budget and abort in flight", async () => {
  let signal;
  const opts = options(async (_, port) => {
    if (port === 44772) await new Promise(resolve => setTimeout(resolve, 50));
    return { endpoint: "localhost:44772", headers: {} };
  });
  opts.adapterFactory.createExecdStack = () => ({ health: { ping: s => { signal = s; return new Promise(() => {}); } } });
  const start = performance.now();
  await assert.rejects(Sandbox.connect(opts), SandboxReadyTimeoutException);
  assert.ok(performance.now() - start < 145);
  assert.equal(signal.aborted, true);
});


test("connect and standalone health checks share probe retry semantics", async () => {
  for (const standalone of [false, true]) {
    let attempts = 0;
    const healthCheck = async () => {
      if (++attempts === 1) throw new SandboxReadyTimeoutException({ message: "a nested probe timed out" });
      return true;
    };
    const sb = await Sandbox.connect(options(async () => ({ endpoint: "localhost:44772", headers: {} }), {
      healthCheck, skipHealthCheck: standalone,
    }));
    if (standalone) await sb.waitUntilReady({ readyTimeoutSeconds: 0.1, pollingIntervalMillis: 1, healthCheck });
    assert.equal(attempts, 2);
    await sb.close();
  }
});


test("health timeout does not report a recovered endpoint error", async (t) => {
  let now = 0;
  t.mock.method(performance, "now", () => now);
  t.mock.method(globalThis, "setTimeout", (callback, delay) => schedule(() => {
    // Exercise a timer firing just before the monotonic deadline.
    now += delay >= 1 ? delay - 0.25 : delay;
    callback();
  }, 0));
  let attempts = 0;
  const opts = options(async () => {
    if (++attempts === 1) throw unavailable();
    return { endpoint: "localhost:44772", headers: {} };
  }, { healthCheck: () => new Promise(() => {}) });
  await assert.rejects(Sandbox.connect(opts), error => {
    assert.ok(error instanceof SandboxReadyTimeoutException);
    assert.equal(error.cause, undefined);
    assert.doesNotMatch(error.message, /starting/);
    assert.match(error.message, /health check timed out/);
    return true;
  });
});

test("late endpoint rejection is handled after timeout or caller abort", async () => {
  for (const cancel of [false, true]) {
    const controller = new AbortController();
    const reason = new Error("caller stopped");
    let rejectEndpoint;
    const endpoint = new Promise((_, reject) => { rejectEndpoint = reject; });
    const connecting = Sandbox.connect(options(() => endpoint, { signal: controller.signal }));
    const rejected = assert.rejects(connecting, error =>
      cancel ? error === reason : error instanceof SandboxReadyTimeoutException);
    if (cancel) controller.abort(reason);
    await rejected;
    rejectEndpoint(new Error("late endpoint failure"));
    // node:test fails on unhandled rejections, including after a test completes.
    await new Promise(resolve => setImmediate(resolve));
  }
});
