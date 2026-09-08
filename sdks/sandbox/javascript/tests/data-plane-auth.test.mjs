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

import assert from "node:assert/strict";
import test from "node:test";

import { ConnectionConfig, DefaultAdapterFactory } from "../dist/index.js";

const API_KEY_HEADER = "open-sandbox-api-key";

function createConfig(useServerProxy, requests, includeExplicitApiKey = false) {
  const fetchImpl = async (input, init) => {
    const request = new Request(input, init);
    requests.push(request);

    if (new URL(request.url).pathname === "/ping") {
      return new Response("", { status: 200 });
    }
    if (new URL(request.url).pathname === "/policy") {
      return Response.json({ policy: { defaultAction: "deny", egress: [] } });
    }
    return Response.json({
      id: "sandbox-1",
      status: { state: "Running" },
      entrypoint: ["sleep", "infinity"],
      createdAt: "2026-09-01T00:00:00Z",
      expiresAt: null,
    });
  };

  const headers = { "X-Custom-Header": "custom-value" };
  if (includeExplicitApiKey) {
    headers["open-sandbox-api-key"] = "explicit-secret";
  }

  const config = new ConnectionConfig({
    domain: "api.opensandbox.test",
    apiKey: "tenant-secret",
    headers,
    useServerProxy,
  });
  config._fetch = fetchImpl;
  config._sseFetch = fetchImpl;
  return config;
}

async function sendDataPlaneRequests(useServerProxy, useEndpointApiKeys = false) {
  const requests = [];
  const connectionConfig = createConfig(
    useServerProxy,
    requests,
    !useServerProxy,
  );
  const factory = new DefaultAdapterFactory();
  const execd = factory.createExecdStack({
    connectionConfig,
    execdBaseUrl: "http://execd.opensandbox.test",
    endpointHeaders: {
      "X-Endpoint-Token": "execd-token",
      ...(useEndpointApiKeys
        ? { "open-sandbox-api-key": "execd-secret" }
        : {}),
    },
  });
  const egress = factory.createEgressStack({
    connectionConfig,
    egressBaseUrl: "http://egress.opensandbox.test",
    endpointHeaders: {
      "X-Endpoint-Token": "egress-token",
      ...(useEndpointApiKeys
        ? { "open-sandbox-api-key": "egress-secret" }
        : {}),
    },
  });

  await execd.health.ping();
  await egress.egress.getPolicy();
  return requests;
}

test("direct execd and egress requests omit the tenant API key", async () => {
  const requests = await sendDataPlaneRequests(false);

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.headers.has(API_KEY_HEADER), false);
    assert.equal(request.headers.get("x-custom-header"), "custom-value");
    assert.ok(request.headers.get("x-endpoint-token"));
  }
});

test("server-proxied execd and egress requests retain the tenant API key", async () => {
  const requests = await sendDataPlaneRequests(true);

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.headers.get(API_KEY_HEADER), "tenant-secret");
  }
});

test("server-proxied requests preserve endpoint-specific API keys", async () => {
  const requests = await sendDataPlaneRequests(true, true);

  assert.equal(requests.length, 2);
  assert.equal(requests[0].headers.get(API_KEY_HEADER), "execd-secret");
  assert.equal(requests[1].headers.get(API_KEY_HEADER), "egress-secret");
});

test("lifecycle requests retain the tenant API key in direct mode", async () => {
  const requests = [];
  const connectionConfig = createConfig(false, requests);
  const lifecycle = new DefaultAdapterFactory().createLifecycleStack({
    connectionConfig,
    lifecycleBaseUrl: connectionConfig.getBaseUrl(),
  });

  await lifecycle.sandboxes.getSandbox("sandbox-1");

  assert.equal(requests.length, 1);
  assert.equal(requests[0].headers.get(API_KEY_HEADER), "tenant-secret");
});
