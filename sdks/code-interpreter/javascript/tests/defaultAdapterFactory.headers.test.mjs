import assert from "node:assert/strict";
import test from "node:test";

import { DefaultAdapterFactory } from "../dist/index.js";

test("DefaultAdapterFactory merges sandbox and endpoint headers for code requests", async () => {
  const recorded = [];
  const fetchImpl = async (input, init = {}) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    const headers = Object.fromEntries(request.headers.entries());
    recorded.push({
      url: request.url,
      method: request.method,
      headers,
    });

    if (url.pathname === "/code/context") {
      return new Response(JSON.stringify({ id: "ctx-1", language: "python" }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    }

    return new Response(
      [
        JSON.stringify({ type: "stdout", text: "hello", timestamp: 1 }),
        JSON.stringify({ type: "execution_complete", execution_time: 2, timestamp: 2 }),
      ].join("\n"),
      {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      }
    );
  };

  const sandbox = {
    connectionConfig: {
      headers: { "x-global": "global" },
      fetch: fetchImpl,
      sseFetch: fetchImpl,
    },
  };

  const factory = new DefaultAdapterFactory();
  const codes = factory.createCodes({
    sandbox,
    execdBaseUrl: "http://sandbox.internal:3456",
    endpointHeaders: { "x-endpoint": "endpoint" },
  });

  const context = await codes.createContext("python");
  assert.equal(context.id, "ctx-1");

  const execution = await codes.run("print('hello')");
  assert.equal(execution.logs.stdout[0]?.text, "hello");

  assert.equal(recorded.length, 2);
  assert.equal(recorded[0].url, "http://sandbox.internal:3456/code/context");
  assert.equal(recorded[0].headers["x-global"], "global");
  assert.equal(recorded[0].headers["x-endpoint"], "endpoint");
  assert.equal(recorded[1].url, "http://sandbox.internal:3456/code");
  assert.equal(recorded[1].headers["x-global"], "global");
  assert.equal(recorded[1].headers["x-endpoint"], "endpoint");
  assert.equal(recorded[1].headers.accept, "text/event-stream");
});
