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

import { ConnectionConfig } from "@alibaba-group/opensandbox";
import { DefaultAdapterFactory } from "../dist/index.js";

const API_KEY_HEADER = "open-sandbox-api-key";

function createConfig(useServerProxy, requests, includeExplicitApiKey = false) {
  const fetchImpl = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    requests.push(request);

    if (new URL(request.url).pathname === "/code/context") {
      return Response.json({ id: "ctx-1", language: "python" });
    }
    return new Response(
      [
        JSON.stringify({ type: "stdout", text: "hello", timestamp: 1 }),
        JSON.stringify({ type: "execution_complete", execution_time: 2, timestamp: 2 }),
      ].join("\n"),
      {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      },
    );
  };

  const headers = { "x-custom-header": "custom-value" };
  if (includeExplicitApiKey) {
    headers["OPEN-SANDBOX-API-KEY"] = "explicit-secret";
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

async function sendCodeRequests(
  useServerProxy,
  useEndpointApiKey = false,
  useExplicitConnectionApiKey = false,
) {
  const requests = [];
  const connectionConfig = createConfig(
    useServerProxy,
    requests,
    !useServerProxy || useExplicitConnectionApiKey,
  );
  const factory = new DefaultAdapterFactory();
  const codes = factory.createCodes({
    sandbox: { connectionConfig },
    execdBaseUrl: "http://execd.opensandbox.test",
    endpointHeaders: {
      "x-endpoint-token": "execd-token",
      ...(useEndpointApiKey ? { [API_KEY_HEADER]: "endpoint-secret" } : {}),
    },
  });

  await codes.createContext("python");
  await codes.run("print('hello')");
  return requests;
}

test("direct code requests omit the tenant API key", async () => {
  const requests = await sendCodeRequests(false);

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.headers.has(API_KEY_HEADER), false);
    assert.equal(request.headers.get("x-custom-header"), "custom-value");
    assert.ok(request.headers.get("x-endpoint-token"));
  }
});

test("server-proxied code requests retain the tenant API key", async () => {
  const requests = await sendCodeRequests(true);

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.headers.get(API_KEY_HEADER), "tenant-secret");
  }
});

test("server-proxied code requests preserve endpoint-specific API keys", async () => {
  const requests = await sendCodeRequests(true, true);

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.headers.get(API_KEY_HEADER), "endpoint-secret");
  }
});

test("server-proxied code requests keep an explicitly configured API key", async () => {
  const requests = await sendCodeRequests(true, false, true);

  assert.equal(requests.length, 2);
  for (const request of requests) {
    assert.equal(request.headers.get(API_KEY_HEADER), "explicit-secret");
  }
});
