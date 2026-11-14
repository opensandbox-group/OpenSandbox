// Copyright 2026 The OpenSandbox Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import assert from "node:assert/strict";
import test from "node:test";
import { ConnectionConfig } from "../dist/index.js";
import { createLifecycleClient, createDiagnosticClient } from "../dist/internal.js";

for (const headerName of ["OPEN-SANDBOX-API-KEY", "open-sandbox-api-key", "Open-Sandbox-Api-Key"]) {
  test(`ConnectionConfig preserves an explicit ${headerName}`, () => {
    const headers = { [headerName]: "explicit-key", "X-Custom": "kept" };
    const config = new ConnectionConfig({ apiKey: "fallback-key", headers });
    for (const candidate of [config, config.withCreateMetricsDisabled()]) {
      assert.equal(new Headers(candidate.headers).get("open-sandbox-api-key"), "explicit-key");
      assert.equal(new Headers(candidate.headers).get("x-custom"), "kept");
    }
    assert.deepEqual(headers, { [headerName]: "explicit-key", "X-Custom": "kept" });
  });

  for (const [name, factory, path] of [
    ["lifecycle", createLifecycleClient, "/sandboxes"],
    ["diagnostic", createDiagnosticClient, "/diagnostics/context"],
  ]) {
    test(`${name} sends only the explicit ${headerName}`, async () => {
      const requests = [];
      const client = factory({
        apiKey: "fallback-key",
        headers: { [headerName]: "explicit-key" },
        fetch: async (request) => {
          requests.push(request);
          return Response.json({});
        },
      });
      await client.GET(path);
      assert.equal(requests.length, 1);
      assert.equal(requests[0].headers.get("open-sandbox-api-key"), "explicit-key");
    });
  }
}

test("missing API key header still uses the configured fallback", async () => {
  const config = new ConnectionConfig({ apiKey: "fallback-key" });
  assert.equal(new Headers(config.headers).get("open-sandbox-api-key"), "fallback-key");
  const client = createLifecycleClient({
    apiKey: "fallback-key",
    fetch: async (request) => {
      assert.equal(request.headers.get("open-sandbox-api-key"), "fallback-key");
      return Response.json({});
    },
  });
  await client.GET("/sandboxes");
});
