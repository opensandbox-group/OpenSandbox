import assert from "node:assert/strict";
import test from "node:test";

import { DefaultAdapterFactory, SupportedLanguages } from "../dist/index.js";

test("DefaultAdapterFactory exposes context CRUD and interrupt operations", async () => {
  const recorded = [];
  const fetchImpl = async (input, init = {}) => {
    const request = input instanceof Request ? input : new Request(input, init);
    const url = new URL(request.url);
    recorded.push({
      method: request.method,
      url: request.url,
      headers: Object.fromEntries(request.headers.entries()),
    });

    if (url.pathname === "/code/context" && request.method === "POST") {
      return Response.json({ id: "ctx-1", language: "python" });
    }
    if (url.pathname === "/code/contexts/ctx-1" && request.method === "GET") {
      return Response.json({ id: "ctx-1", language: "python" });
    }
    if (url.pathname === "/code/contexts" && request.method === "GET") {
      const language = url.searchParams.get("language");
      return Response.json(
        language
          ? [{ id: "ctx-2", language }]
          : [
              { id: "ctx-1", language: "python" },
              { id: "ctx-2", language: "go" },
            ],
      );
    }
    if (url.pathname === "/code/contexts/ctx-1" && request.method === "DELETE") {
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/code/contexts" && request.method === "DELETE") {
      return new Response(null, { status: 204 });
    }
    if (url.pathname === "/code" && request.method === "DELETE") {
      return new Response(null, { status: 204 });
    }
    throw new Error(`Unexpected request: ${request.method} ${request.url}`);
  };

  const factory = new DefaultAdapterFactory();
  const codes = factory.createCodes({
    sandbox: {
      connectionConfig: {
        headers: { "x-global": "global" },
        fetch: fetchImpl,
        sseFetch: fetchImpl,
      },
    },
    execdBaseUrl: "http://sandbox.internal:3456",
    endpointHeaders: { "x-endpoint": "endpoint" },
  });

  const created = await codes.createContext(SupportedLanguages.PYTHON);
  const fetched = await codes.getContext("ctx-1");
  const allContexts = await codes.listContexts();
  const goContexts = await codes.listContexts(SupportedLanguages.GO);
  await codes.deleteContext("ctx-1");
  await codes.deleteContexts(SupportedLanguages.GO);
  await codes.interrupt("exec-1");

  assert.deepEqual(created, { id: "ctx-1", language: "python" });
  assert.deepEqual(fetched, { id: "ctx-1", language: "python" });
  assert.equal(allContexts.length, 2);
  assert.deepEqual(goContexts, [{ id: "ctx-2", language: "go" }]);
  assert.deepEqual(
    recorded.map((entry) => `${entry.method} ${entry.url}`),
    [
      "POST http://sandbox.internal:3456/code/context",
      "GET http://sandbox.internal:3456/code/contexts/ctx-1",
      "GET http://sandbox.internal:3456/code/contexts",
      "GET http://sandbox.internal:3456/code/contexts?language=go",
      "DELETE http://sandbox.internal:3456/code/contexts/ctx-1",
      "DELETE http://sandbox.internal:3456/code/contexts?language=go",
      "DELETE http://sandbox.internal:3456/code?id=exec-1",
    ],
  );
  for (const entry of recorded) {
    assert.equal(entry.headers["x-global"], "global");
    assert.equal(entry.headers["x-endpoint"], "endpoint");
  }
});
