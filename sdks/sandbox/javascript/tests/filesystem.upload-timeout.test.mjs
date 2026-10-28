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
import http from "node:http";
import { setTimeout as sleep } from "node:timers/promises";
import test from "node:test";

import { ConnectionConfig, DefaultAdapterFactory } from "../dist/index.js";
import {
  FilesystemAdapter,
  IsolatedSessionsAdapter,
  createExecdClient,
  _setClientIpForTest,
} from "../dist/internal.js";

// Regression: an upload whose request body takes longer than
// requestTimeoutSeconds must still complete. The request timeout bounds the
// wait for the server's response; it must not abort a caller-sized body while
// it is still being sent, and once the body has been sent it must still bound
// the wait for response headers.

const REQUEST_TIMEOUT_SECONDS = 0.25;
const CHUNK_DELAY_MS = 120;
const CHUNK_COUNT = 5; // ~600 ms of body, well past the 250 ms request timeout
const BASE_URL = "http://127.0.0.1:8080";

_setClientIpForTest("127.0.0.1");

async function withServer(handler, run) {
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const url = `http://127.0.0.1:${server.address().port}`;
  try {
    return await run(url);
  } finally {
    server.closeAllConnections?.();
    await new Promise((resolve) => server.close(resolve));
  }
}

function filesAdapter(url) {
  const connectionConfig = new ConnectionConfig({
    domain: url.replace(/^http:\/\//, ""),
    protocol: "http",
    requestTimeoutSeconds: REQUEST_TIMEOUT_SECONDS,
  }).withTransportIfMissing();
  return new DefaultAdapterFactory().createExecdStack({
    connectionConfig,
    execdBaseUrl: url,
  }).files;
}

async function readBody(req, { stallMs = 0, stallCount = 0 } = {}) {
  let bytes = 0;
  const counted = new Promise((resolve) => {
    req.on("data", (chunk) => {
      bytes += chunk.length;
    });
    req.on("end", () => resolve(bytes));
    // The client may abort mid-body when the regression is present.
    req.on("aborted", () => resolve(bytes));
    req.on("error", () => resolve(bytes));
  });
  for (let i = 0; i < stallCount; i += 1) {
    req.pause();
    await sleep(stallMs);
    req.resume();
  }
  return await counted;
}

async function* slowBody() {
  for (let i = 0; i < CHUNK_COUNT; i += 1) {
    await sleep(CHUNK_DELAY_MS);
    yield new TextEncoder().encode(`chunk-${i}`);
  }
}

async function* streamOf(text) {
  yield new TextEncoder().encode(text);
}

test("streamed upload survives a body transfer longer than requestTimeoutSeconds", async () => {
  let received = 0;
  await withServer(
    async (req, res) => {
      received = await readBody(req);
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
    },
    async (url) => {
      await filesAdapter(url).writeFiles([
        { path: "/tmp/big.bin", data: slowBody() },
      ]);
    },
  );
  assert.ok(received > 0, "the server should have received the upload");
});

test("in-memory upload survives a body transfer longer than requestTimeoutSeconds", async () => {
  // 8 MiB cannot fit into the socket buffers. The server keeps stalling in
  // short steps that stay under the request timeout but add up past it, so the
  // transfer outlives the timeout while data keeps flowing.
  const payload = new Uint8Array(8 * 1024 * 1024);
  let received = 0;
  await withServer(
    async (req, res) => {
      received = await readBody(req, { stallMs: 100, stallCount: 4 });
      res.writeHead(200, { "content-type": "application/json" });
      res.end("{}");
    },
    async (url) => {
      await filesAdapter(url).writeFiles([
        { path: "/tmp/big.bin", data: payload },
      ]);
    },
  );
  assert.ok(received > 0, "the server should have received the upload");
});

test(
  "upload fails on the response-header deadline when the server never responds",
  { timeout: 5000 },
  async () => {
    const started = Date.now();
    await assert.rejects(
      withServer(
        async (req) => {
          // Consume the body, then never send response headers.
          await readBody(req);
        },
        async (url) => {
          await filesAdapter(url).writeFiles([
            { path: "/tmp/stuck.bin", data: "hello" },
          ]);
        },
      ),
      (error) => {
        const elapsed = Date.now() - started;
        assert.ok(
          elapsed < 4000,
          `expected the response-header deadline to fire, took ${elapsed}ms`
        );
        assert.match(
          `${error?.message ?? ""} ${error?.cause?.code ?? ""}`,
          /HEADERS_TIMEOUT|headers timeout|fetch failed/i
        );
        return true;
      },
    );
  },
);

test("requestTimeoutSeconds = 0 disables the upload response-header deadline", async () => {
  // undici's documented disable value for `headersTimeout` is 0; passing
  // `null` (or omitting it) silently falls back to undici's 300s default, so
  // an upload against a wedged server would still abort after ~5 minutes.
  // Pin the value the SDK hands to undici: it must be 0, not null.
  const originalFetch = globalThis.fetch;
  const capturedDispatchers = [];
  globalThis.fetch = async (_input, init) => {
    capturedDispatchers.push(init?.dispatcher);
    return new Response("{}", {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };

  const connectionConfig = new ConnectionConfig({
    domain: BASE_URL.replace(/^http:\/\//, ""),
    protocol: "http",
    requestTimeoutSeconds: 0,
  }).withTransportIfMissing();

  try {
    await connectionConfig.uploadFetch(`${BASE_URL}/v1/files`, {
      method: "POST",
      body: "hello",
    });
    await connectionConfig.fetch(`${BASE_URL}/v1/files`, { method: "GET" });
  } finally {
    globalThis.fetch = originalFetch;
    await connectionConfig.closeTransport();
  }

  const headersTimeoutOf = (dispatcher) => {
    assert.ok(
      dispatcher,
      "expected the SDK to route through a Node dispatcher",
    );
    const options = Object.getOwnPropertySymbols(dispatcher)
      .map((symbol) => dispatcher[symbol])
      .find(
        (value) =>
          value && typeof value === "object" && "keepAliveTimeout" in value,
      );
    assert.ok(options, "undici Agent options should be present");
    return options.headersTimeout;
  };

  assert.equal(capturedDispatchers.length, 2);
  assert.equal(
    headersTimeoutOf(capturedDispatchers[0]),
    0,
    "uploads with requestTimeoutSeconds = 0 must pass undici's disable value 0, not null",
  );
  assert.equal(
    headersTimeoutOf(capturedDispatchers[1]),
    undefined,
    "non-upload calls should keep undici's default headers timeout",
  );
});

test("file calls that are not uploads keep the request timeout", { timeout: 5000 }, async () => {
  await assert.rejects(
    withServer(
      () => {
        // Accept the request but never send response headers.
      },
      async (url) => {
        await filesAdapter(url).readBytes("/tmp/big.bin");
      },
    ),
    (error) => {
      assert.match(String(error?.message), /timed out/);
      return true;
    },
  );
});

test("upload paths use the upload fetch while other file calls keep the timed fetch", async () => {
  const calls = [];
  const jsonResponse = () =>
    new Response("{}", {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  const timedFetch = async (input, init) => {
    calls.push(`timed:${init?.method ?? "GET"}`);
    return jsonResponse();
  };
  const uploadFetch = async (input, init) => {
    calls.push(`upload:${init?.method ?? "GET"}`);
    return jsonResponse();
  };
  const client = createExecdClient({ baseUrl: BASE_URL, fetch: timedFetch });
  const adapter = new FilesystemAdapter(client, {
    baseUrl: BASE_URL,
    fetch: timedFetch,
    uploadFetch,
  });

  await adapter.writeFiles([{ path: "/tmp/a.txt", data: "hello" }]);
  await adapter.writeFiles([{ path: "/tmp/a.txt", data: streamOf("hello") }]);
  await adapter.readBytes("/tmp/a.txt");

  assert.deepEqual(calls, ["upload:POST", "upload:POST", "timed:GET"]);
});

test("isolated session uploads use the upload fetch and fall back to fetch without one", async () => {
  const jsonResponse = () =>
    new Response("{}", {
      status: 200,
      headers: { "content-type": "application/json" },
    });

  const calls = [];
  const timedFetch = async (input, init) => {
    calls.push(`timed:${init?.method ?? "GET"}`);
    return jsonResponse();
  };
  const uploadFetch = async (input, init) => {
    calls.push(`upload:${init?.method ?? "GET"}`);
    return jsonResponse();
  };

  const isolated = new IsolatedSessionsAdapter({
    baseUrl: BASE_URL,
    fetch: timedFetch,
    uploadFetch,
  });
  const session = await isolated.attach("session-1");
  await session.files.writeFiles([{ path: "/tmp/a.txt", data: "hello" }]);
  assert.deepEqual(calls, ["timed:GET", "upload:POST"]);

  const fallbackCalls = [];
  const fallbackFetch = async (input, init) => {
    fallbackCalls.push(`timed:${init?.method ?? "GET"}`);
    return jsonResponse();
  };
  const isolatedFallback = new IsolatedSessionsAdapter({
    baseUrl: BASE_URL,
    fetch: fallbackFetch,
  });
  const fallbackSession = await isolatedFallback.attach("session-1");
  await fallbackSession.files.writeFiles([{ path: "/tmp/a.txt", data: "hello" }]);
  assert.deepEqual(fallbackCalls, ["timed:GET", "timed:POST"]);
});
