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

import { propagation, trace } from "@opentelemetry/api";
import { SandboxPool } from "../dist/index.js";

const spans = [];

class RecordingSpan {
  constructor(name, attributes) {
    this.name = name;
    this.attributes = { ...(attributes ?? {}) };
    this.ended = false;
  }

  setAttribute(name, value) { this.attributes[name] = value; return this; }
  setStatus() { return this; }
  recordException() {}
  end() { this.ended = true; }
}

trace.setGlobalTracerProvider({
  getTracer() {
    return {
      startActiveSpan(name, options, callback) {
        const span = new RecordingSpan(name, options?.attributes);
        spans.push(span);
        return callback(span);
      },
    };
  },
});

propagation.setGlobalPropagator({
  fields: () => ["traceparent"],
  inject(_context, carrier, setter) {
    setter.set(carrier, "traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01");
  },
  extract(context) { return context; },
});

async function eventually(check, timeoutMs = 2_000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  assert.fail("condition did not become true before timeout");
}

function sandbox(id) {
  return {
    id,
    async isHealthy() { return true; },
    async renew() {},
    async kill() {},
    async close() {},
  };
}

test("opt-in warmup tracing emits Kotlin-compatible phase spans and propagates context", async () => {
  spans.length = 0;
  let createHeaders;
  const pool = SandboxPool.create({
    poolName: "traced-pool",
    maxIdle: 1,
    connectionConfig: { domain: "localhost:8080", enableTracing: true },
    creationSpec: { image: "test" },
    sandboxCreator: async (context) => {
      createHeaders = context.createConnectionConfig.headers;
      return sandbox("traced");
    },
    warmupHealthCheck: async () => true,
    warmupSandboxPreparer: async () => {},
    warmupPostPrepareHealthCheck: async () => true,
  });

  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    assert.deepEqual(spans.map((span) => span.name), [
      "pool.warmup",
      "pool.warmup.create",
      "pool.warmup.readiness",
      "pool.warmup.prepare",
      "pool.warmup.post_prepare_readiness",
      "pool.warmup.renew",
      "pool.warmup.commit",
    ]);
    assert.ok(spans.every((span) => span.ended));
    assert.equal(spans[0].attributes["pool.name"], "traced-pool");
    assert.equal(
      createHeaders.traceparent,
      "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
    );
  } finally {
    await pool.shutdown(false);
  }
});

test("disabled tracing emits no warmup spans", async () => {
  spans.length = 0;
  const pool = SandboxPool.create({
    poolName: "untraced-pool",
    maxIdle: 1,
    connectionConfig: { domain: "localhost:8080" },
    creationSpec: { image: "test" },
    sandboxCreator: async () => sandbox("untraced"),
    warmupSkipHealthCheck: true,
  });
  try {
    await pool.start();
    await eventually(async () => (await pool.snapshot()).idleCount === 1);
    assert.deepEqual(spans, []);
  } finally {
    await pool.shutdown(false);
  }
});
