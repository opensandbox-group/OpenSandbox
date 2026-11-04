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
import { Sandbox, SandboxManager, ConnectionConfig, SandboxApiException } from "../dist/index.js";
import { createDiagnosticClient, createLifecycleClient } from "../dist/internal.js";

const context = { sandboxId: "sbx-1", backendType: "kubernetes", backendIdentifiers: { namespace: "tenant-ns" } };

/**
 * Normalize a transport call into a Request. The redirect-guarded lifecycle
 * transport dispatches hops as `(url, init)` — valid per the fetch contract —
 * while unguarded paths pass a Request, so these mocks must not assume either.
 */
function toRequest(input, init) {
  return input instanceof Request ? input : new Request(input, init);
}

function mockConfig(handler, options = {}) {
  const config = new ConnectionConfig({
    domain: "localhost:8080", protocol: "http", apiKey: "key", headers: { "x-custom": "custom" },
    ...options,
  });
  Object.defineProperty(config, "fetch", { value: handler });
  config.withTransportIfMissing = () => config;
  return config;
}

test("manager diagnostics uses lifecycle transport and preserves descriptor metadata", async () => {
  const calls = [];
  const manager = SandboxManager.create({ connectionConfig: mockConfig(
    async (input, init) => {
      const req = toRequest(input, init);
      calls.push(req);
      assert.equal(req.headers.get("OPEN-SANDBOX-API-KEY"), "key");
      assert.equal(req.headers.get("x-custom"), "custom");
      const url = new URL(req.url);
      if (url.pathname.endsWith("/context")) return Response.json(context);
      assert.equal(url.searchParams.get("scope"), "all");
      const kind = url.pathname.endsWith("/logs") ? "logs" : "events";
      return Response.json({
        sandboxId: "sbx-1", kind, scope: "all", delivery: "url", contentType: "text/plain",
        contentUrl: "https://example.invalid/content", expiresAt: "2026-10-19T12:00:00Z",
        truncated: true, warnings: ["partial"],
      });
    },
  ) });
  try {
    assert.deepEqual(await manager.getDiagnosticContext("sbx-1"), context);
    for (const kind of ["Logs", "Events"]) {
      const result = await manager[`getDiagnostic${kind}`]("sbx-1", "all");
      assert.equal(result.delivery, "url");
      assert.equal(result.expiresAt.toISOString(), "2026-10-19T12:00:00.000Z");
      assert.deepEqual(result.warnings, ["partial"]);
      assert.equal(result.truncated, true);
    }
    assert.equal(calls.length, 3); // No execd connection or download of contentUrl.
  } finally { await manager.close(); }
});

for (const source of ["config", "environment"]) {
  test(`diagnostics preserves explicit auth header over ${source} API key`, async () => {
    const previousKey = process.env.OPEN_SANDBOX_API_KEY;
    process.env.OPEN_SANDBOX_API_KEY = "environment-key";
    const calls = [];
    const manager = SandboxManager.create({ connectionConfig: mockConfig(async (input, init) => {
      const req = toRequest(input, init);
      calls.push(req);
      assert.equal(req.headers.get("OPEN-SANDBOX-API-KEY"), "explicit-header-key");
      if (new URL(req.url).pathname.endsWith("/context")) return Response.json(context);
      return Response.json({ code: "NOT_FOUND", message: "Not found" }, { status: 404 });
    }, {
      apiKey: source === "config" ? "config-key" : undefined,
      headers: { "OPEN-SANDBOX-API-KEY": "explicit-header-key" },
    }) });
    try {
      await assert.rejects(manager.getSandboxInfo("sbx-1"), SandboxApiException);
      assert.deepEqual(await manager.getDiagnosticContext("sbx-1"), context);
      assert.equal(calls.length, 2);
    } finally {
      await manager.close();
      if (previousKey === undefined) delete process.env.OPEN_SANDBOX_API_KEY;
      else process.env.OPEN_SANDBOX_API_KEY = previousKey;
    }
  });
}

test("sandbox diagnostics delegates through the injected lifecycle stack", async () => {
  const calls = [];
  const diagnostic = {
    getContext: async (id) => { calls.push([id]); return context; },
    getLogs: async (...args) => { calls.push(args); return {}; },
    getEvents: async (...args) => { calls.push(args); return {}; },
  };
  const config = new ConnectionConfig({ domain: "localhost:8080" });
  const sandbox = await Sandbox.connect({
    sandboxId: "sbx-1", connectionConfig: config, skipHealthCheck: true,
    adapterFactory: {
      createLifecycleStack: () => ({
        sandboxes: { getSandboxEndpoint: async () => ({ endpoint: "localhost:44772" }) },
        diagnostics: diagnostic,
      }),
      createExecdStack: () => ({ commands: {}, files: {}, health: {}, metrics: {} }),
      createEgressStack: () => ({ egress: {} }),
    },
  });
  try {
    assert.deepEqual(await sandbox.getDiagnosticContext(), context);
    await sandbox.getDiagnosticLogs("container");
    await sandbox.getDiagnosticEvents("runtime");
    assert.deepEqual(calls, [["sbx-1"], ["sbx-1", "container"], ["sbx-1", "runtime"]]);
  } finally { await sandbox.close(); }
});

test("diagnostic and lifecycle clients share environment and URL defaults", async () => {
  const previousKey = process.env.OPEN_SANDBOX_API_KEY;
  process.env.OPEN_SANDBOX_API_KEY = "environment-key";
  const calls = [];
  const fetch = async (input, init) => {
    const req = toRequest(input, init);
    calls.push(req);
    assert.equal(req.headers.get("OPEN-SANDBOX-API-KEY"), "environment-key");
    return Response.json(context);
  };
  try {
    await createLifecycleClient({ fetch }).GET("/sandboxes/{sandboxId}", {
      params: { path: { sandboxId: "sbx-1" } },
    });
    await createDiagnosticClient({ fetch }).GET("/sandboxes/{sandboxId}/diagnostics/context", {
      params: { path: { sandboxId: "sbx-1" } },
    });
    assert.deepEqual(calls.map((req) => req.url), [
      "http://localhost:8080/v1/sandboxes/sbx-1",
      "http://localhost:8080/v1/sandboxes/sbx-1/diagnostics/context",
    ]);
  } finally {
    if (previousKey === undefined) delete process.env.OPEN_SANDBOX_API_KEY;
    else process.env.OPEN_SANDBOX_API_KEY = previousKey;
  }
});

test("inline diagnostics preserves content without requiring an expiry", async () => {
  for (const expiry of [{}, { expiresAt: null }]) {
    const manager = SandboxManager.create({ connectionConfig: mockConfig(async (input, init) => {
      const req = toRequest(input, init);
      const kind = new URL(req.url).pathname.endsWith("/logs") ? "logs" : "events";
      return Response.json({
        sandboxId: "sbx-1", kind, scope: "all", delivery: "inline", contentType: "text/plain",
        content: "hello\n", contentLength: 6, truncated: false, ...expiry,
      });
    }) });
    try {
      for (const kind of ["Logs", "Events"]) {
        const result = await manager[`getDiagnostic${kind}`]("sbx-1", "all");
        assert.equal(result.delivery, "inline");
        assert.equal(result.content, "hello\n");
        assert.equal(result.contentLength, 6);
        assert.equal(result.expiresAt, undefined);
      }
    } finally { await manager.close(); }
  }
});

test("custom factories without diagnostics produce an actionable error", async () => {
  const config = new ConnectionConfig({ domain: "localhost:8080" });
  const adapterFactory = {
    createLifecycleStack: () => ({
      sandboxes: { getSandboxEndpoint: async () => ({ endpoint: "localhost:44772" }) },
    }),
    createExecdStack: () => ({ commands: {}, files: {}, health: {}, metrics: {} }),
    createEgressStack: () => ({ egress: {} }),
  };
  const manager = SandboxManager.create({ connectionConfig: config, adapterFactory });
  const sandbox = await Sandbox.connect({
    sandboxId: "sbx-1", connectionConfig: config, skipHealthCheck: true, adapterFactory,
  });
  try {
    for (const call of [
      () => manager.getDiagnosticContext("sbx-1"),
      () => manager.getDiagnosticLogs("sbx-1", "all"),
      () => manager.getDiagnosticEvents("sbx-1", "all"),
      () => sandbox.getDiagnosticContext(),
      () => sandbox.getDiagnosticLogs("all"),
      () => sandbox.getDiagnosticEvents("all"),
    ]) {
      assert.throws(call, /The adapter factory does not provide diagnostics/);
    }
  } finally {
    await sandbox.close();
    await manager.close();
  }
});

test("diagnostics preserves API errors", async () => {
  for (const status of [403, 404, 501]) {
    const manager = SandboxManager.create({ connectionConfig: mockConfig(
      async () => Response.json(
        { code: "TEST_ERROR", message: "Unavailable" }, { status, headers: { "x-request-id": "request-1" } },
      ),
    ) });
    try {
      for (const call of [
        () => manager.getDiagnosticContext("sbx-1"),
        () => manager.getDiagnosticLogs("sbx-1", "all"),
        () => manager.getDiagnosticEvents("sbx-1", "all"),
      ]) {
        await assert.rejects(call, (error) =>
          error instanceof SandboxApiException && error.statusCode === status && error.requestId === "request-1");
      }
    } finally { await manager.close(); }
  }
});
