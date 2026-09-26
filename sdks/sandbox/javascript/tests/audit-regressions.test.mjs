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

// Regression tests for the #1768 SDK audit fixes:
// - instance resume() must not share (or kill) the source sandbox's transport
// - download streams must release the body lock when consumers exit early
// - the endpoint cache must not drop a newer in-flight fetch on stale completion

import assert from "node:assert/strict";
import test from "node:test";

import {
  ConnectionConfig,
  Sandbox,
} from "../dist/index.js";
import { EndpointCache, FilesystemAdapter } from "../dist/internal.js";

function createAdapterFactory(options = {}) {
  const { endpointFailureAfter = Infinity } = options;
  const calls = [];
  let endpointCalls = 0;
  const sandboxes = {
    async getSandboxEndpoint(sandboxId, port, useServerProxy) {
      endpointCalls += 1;
      if (endpointCalls > endpointFailureAfter) {
        throw new Error("simulated endpoint failure");
      }
      calls.push({ method: "getSandboxEndpoint", args: [sandboxId, port, useServerProxy] });
      return {
        endpoint: `sandbox.internal:${port}`,
        headers: { "x-port": String(port) },
      };
    },
    async resumeSandbox(sandboxId) {
      calls.push({ method: "resumeSandbox", args: [sandboxId] });
    },
    async getSandbox() {
      throw new Error("not implemented");
    },
    async listSandboxes() {
      throw new Error("not implemented");
    },
    async createSandbox() {
      throw new Error("not implemented");
    },
    async deleteSandbox() {},
    async pauseSandbox() {},
    async renewSandboxExpiration() {
      throw new Error("not implemented");
    },
  };

  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack(opts) {
      calls.push({ method: "createExecdStack", args: [opts] });
      return {
        commands: { kind: "commands" },
        files: { kind: "files" },
        health: { async ping() { return true; } },
        metrics: { kind: "metrics" },
      };
    },
    createEgressStack(opts) {
      calls.push({ method: "createEgressStack", args: [opts] });
      return {
        egress: {
          async getPolicy() {
            return { defaultAction: "deny", egress: [] };
          },
          async patchRules() {},
        },
      };
    },
  };

  return { adapterFactory, calls };
}

test("instance resume gives the resumed sandbox its own transport", async () => {
  const { adapterFactory } = createAdapterFactory();
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });

  const original = await Sandbox.connect({
    sandboxId: "sbx-resume-own",
    connectionConfig,
    adapterFactory,
    skipHealthCheck: true,
  });
  const resumed = await original.resume({ skipHealthCheck: true });

  // Regression: both instances used to share one undici agent, so closing the
  // original killed the resumed sandbox's connections.
  assert.notEqual(resumed.connectionConfig, original.connectionConfig);
});

test("failed instance resume does not close the original sandbox transport", async () => {
  const { adapterFactory } = createAdapterFactory({ endpointFailureAfter: 2 });

  const original = await Sandbox.connect({
    sandboxId: "sbx-resume-fail",
    connectionConfig: new ConnectionConfig({ domain: "http://127.0.0.1:8080" }),
    adapterFactory,
    skipHealthCheck: true,
  });

  let originalClosed = false;
  const realClose = original.connectionConfig.closeTransport.bind(original.connectionConfig);
  original.connectionConfig.closeTransport = async () => {
    originalClosed = true;
    await realClose();
  };

  // Resume fails at the endpoint lookup (calls 3+); the SDK must tear down
  // only the transport it allocated for the resumed instance.
  await assert.rejects(original.resume({ skipHealthCheck: true }));
  assert.equal(originalClosed, false);

  // The original sandbox is still fully usable.
  assert.equal(await original.isHealthy(), true);
});

test("readBytesStream releases the body lock when the consumer exits early", async () => {
  let cancelled = false;
  const stream = new ReadableStream({
    start(controller) {
      const encoder = new TextEncoder();
      controller.enqueue(encoder.encode("chunk-1"));
      controller.enqueue(encoder.encode("chunk-2"));
      controller.enqueue(encoder.encode("chunk-3"));
    },
    cancel() {
      cancelled = true;
    },
  });

  const adapter = new FilesystemAdapter(null, {
    baseUrl: "http://sandbox.internal:44772",
    headers: {},
    fetch: async () => ({
      ok: true,
      status: 200,
      headers: new Headers(),
      body: stream,
    }),
  });

  for await (const chunk of adapter.readBytesStream("/tmp/file")) {
    void chunk;
    break; // early exit — the body lock used to stay held until GC
  }
  // Allow the async finally block to run.
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(cancelled, true, "reader.cancel() must run on early consumer exit");
});

test("stale in-flight completion does not delete a newer fetch entry", async () => {
  const cache = new EndpointCache(8, 60_000);
  const gates = [];
  const fetchCount = { value: 0 };

  function fetcher() {
    fetchCount.value += 1;
    let release;
    const gate = new Promise((resolve) => {
      release = resolve;
    });
    gates.push(release);
    return gate.then(() => ({ endpoint: `ep-${fetchCount.value}`, headers: {} }));
  }

  const first = cache.getOrFetch("sbx", 44772, false, fetcher);
  // invalidate() drops the first in-flight entry and bumps the generation.
  cache.invalidate("sbx");
  const second = cache.getOrFetch("sbx", 44772, false, fetcher);

  // The first (stale) fetch completes after invalidate: it must not delete
  // the second fetch's in-flight entry, or a third caller would start a
  // duplicate fetch.
  gates[0]({ endpoint: "stale", headers: {} });
  await first.catch(() => undefined);

  gates[1]({ endpoint: "fresh", headers: {} });
  await second;

  // A third caller must be served from the cache, not start a new fetch.
  const before = fetchCount.value;
  await cache.getOrFetch("sbx", 44772, false, fetcher);
  assert.equal(fetchCount.value, before);
  assert.equal(before, 2, "expected exactly two fetches");
});
