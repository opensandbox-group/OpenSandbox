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

import { DefaultAdapterFactory } from "../dist/index.js";

test("code-interpreter requests strip protected headers on cross-origin redirects", async () => {
  const calls = [];
  const fetchImpl = async (input, init = {}) => {
    const request = input instanceof Request ? input : new Request(input, init);
    calls.push({
      url: request.url,
      headers: Object.fromEntries(request.headers.entries()),
    });

    if (new URL(request.url).origin === "http://sandbox.internal:3456") {
      return new Response(null, {
        status: 302,
        headers: { location: "http://other.example/final" },
      });
    }
    return new Response(JSON.stringify({ id: "ctx-1", language: "python" }), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };

  const sandbox = {
    connectionConfig: {
      headers: { "x-global": "global" },
      fetch: fetchImpl,
      sseFetch: fetchImpl,
    },
  };

  const codes = new DefaultAdapterFactory().createCodes({
    sandbox,
    execdBaseUrl: "http://sandbox.internal:3456",
    endpointHeaders: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
  });

  const context = await codes.createContext("python");
  assert.equal(context.id, "ctx-1");

  assert.equal(calls.length, 2);
  // The configured endpoint still receives the protected credential...
  assert.equal(calls[0].headers["open-sandbox-api-key"], "tenant-secret");
  // ...while the cross-origin redirect target does not.
  assert.equal(calls[1].url, "http://other.example/final");
  assert.equal(calls[1].headers["open-sandbox-api-key"], undefined);
});

test("streaming run() strips protected headers on cross-origin redirects", async () => {
  const calls = [];
  const fetchImpl = async (input, init = {}) => {
    const request = input instanceof Request ? input : new Request(input, init);
    calls.push({
      url: request.url,
      headers: Object.fromEntries(request.headers.entries()),
    });

    if (new URL(request.url).origin === "http://sandbox.internal:3456") {
      return new Response(null, {
        status: 302,
        headers: { location: "http://other.example/stream" },
      });
    }
    return new Response(
      [
        JSON.stringify({ type: "stdout", text: "hi", timestamp: 1 }),
        JSON.stringify({ type: "execution_complete", execution_time: 1, timestamp: 2 }),
      ].join("\n"),
      { status: 200, headers: { "content-type": "text/event-stream" } },
    );
  };

  const sandbox = {
    connectionConfig: {
      headers: {},
      fetch: fetchImpl,
      sseFetch: fetchImpl,
    },
  };

  const codes = new DefaultAdapterFactory().createCodes({
    sandbox,
    execdBaseUrl: "http://sandbox.internal:3456",
    endpointHeaders: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
  });

  const execution = await codes.run("print('hi')");
  assert.equal(execution.logs.stdout[0]?.text, "hi");

  assert.equal(calls.length, 2);
  assert.equal(calls[0].headers["open-sandbox-api-key"], "tenant-secret");
  assert.equal(calls[1].url, "http://other.example/stream");
  assert.equal(calls[1].headers["open-sandbox-api-key"], undefined);
});

test("same-origin redirects preserve the credential", async () => {
  const calls = [];
  const fetchImpl = async (input, init = {}) => {
    const request = input instanceof Request ? input : new Request(input, init);
    calls.push({
      url: request.url,
      headers: Object.fromEntries(request.headers.entries()),
    });

    if (calls.length === 1) {
      return new Response(null, {
        status: 302,
        headers: { location: "http://sandbox.internal:3456/code/context2" },
      });
    }
    return new Response(JSON.stringify({ id: "ctx-1", language: "python" }), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };

  const sandbox = {
    connectionConfig: {
      headers: {},
      fetch: fetchImpl,
      sseFetch: fetchImpl,
    },
  };

  const codes = new DefaultAdapterFactory().createCodes({
    sandbox,
    execdBaseUrl: "http://sandbox.internal:3456",
    endpointHeaders: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
  });

  const context = await codes.createContext("python");
  assert.equal(context.id, "ctx-1");

  assert.equal(calls.length, 2);
  assert.equal(calls[1].url, "http://sandbox.internal:3456/code/context2");
  // Same-origin hop keeps the credential.
  assert.equal(calls[1].headers["open-sandbox-api-key"], "tenant-secret");
});
