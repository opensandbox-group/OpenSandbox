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
import test from "node:test";

import {
  ConnectionConfig,
  DefaultAdapterFactory,
  SandboxApiException,
} from "../dist/index.js";
import {
  FilesystemAdapter,
  createExecdClient,
  createRedirectSafeFetch,
  hasProtectedHeaders,
  isSameOrigin,
  stripProtectedHeaders,
} from "../dist/internal.js";

/** Start a throwaway HTTP server on an ephemeral 127.0.0.1 port. */
function startServer(handler) {
  return new Promise((resolve) => {
    const server = http.createServer(handler);
    server.listen(0, "127.0.0.1", () => {
      const { port } = server.address();
      resolve({
        server,
        origin: `http://127.0.0.1:${port}`,
        close: () =>
          new Promise((done) => {
            server.closeAllConnections?.();
            server.close(() => done());
          }),
      });
    });
  });
}

test("protected header helpers are case-insensitive and origin-aware", () => {
  const headers = new Headers({
    "Open-Sandbox-Api-Key": "secret",
    "opensandbox-egress-auth": "egress",
    "X-Custom": "keep",
  });
  assert.equal(hasProtectedHeaders(headers), true);
  stripProtectedHeaders(headers);
  assert.equal(headers.has("open-sandbox-api-key"), false);
  assert.equal(headers.has("opensandbox-egress-auth"), false);
  assert.equal(headers.get("x-custom"), "keep");
  assert.equal(hasProtectedHeaders(new Headers({ "X-Custom": "keep" })), false);

  assert.equal(isSameOrigin(new URL("https://a.test/x"), new URL("https://a.test:443/y")), true);
  assert.equal(isSameOrigin(new URL("http://a.test/x"), new URL("http://a.test:80/y")), true);
  assert.equal(isSameOrigin(new URL("https://a.test"), new URL("http://a.test")), false);
  assert.equal(isSameOrigin(new URL("https://a.test"), new URL("https://a.test:8443")), false);
});

test("cross-origin redirect strips protected headers but keeps other headers", async () => {
  let sourceHeaders;
  let targetHeaders;
  const target = await startServer((req, res) => {
    targetHeaders = req.headers;
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok");
  });
  const source = await startServer((req, res) => {
    sourceHeaders = req.headers;
    res.writeHead(302, { location: `${target.origin}/final` });
    res.end();
  });

  try {
    const safeFetch = createRedirectSafeFetch(source.origin, fetch);
    const res = await safeFetch(`${source.origin}/start`, {
      headers: {
        "OPEN-SANDBOX-API-KEY": "tenant-secret",
        "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
        "X-Custom": "keep-me",
      },
    });

    assert.equal(res.status, 200);
    // Credentials are still sent to the origin they were configured for.
    assert.equal(sourceHeaders["open-sandbox-api-key"], "tenant-secret");
    // ...but never replayed to the redirect target.
    assert.equal(targetHeaders["open-sandbox-api-key"], undefined);
    assert.equal(targetHeaders["opensandbox-egress-auth"], undefined);
    // Non-protected headers are untouched.
    assert.equal(targetHeaders["x-custom"], "keep-me");
  } finally {
    await source.close();
    await target.close();
  }
});

test("same-origin redirect preserves protected headers", async () => {
  let finalHeaders;
  const server = await startServer((req, res) => {
    if (req.url === "/start") {
      res.writeHead(302, { location: "/final" });
      res.end();
      return;
    }
    finalHeaders = req.headers;
    res.writeHead(200);
    res.end("ok");
  });

  try {
    const safeFetch = createRedirectSafeFetch(server.origin, fetch);
    const res = await safeFetch(`${server.origin}/start`, {
      headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
    });
    assert.equal(res.status, 200);
    assert.equal(finalHeaders["open-sandbox-api-key"], "tenant-secret");
  } finally {
    await server.close();
  }
});

test("requests without protected headers keep default follow behavior", async () => {
  let targetHeaders;
  const target = await startServer((req, res) => {
    targetHeaders = req.headers;
    res.writeHead(200);
    res.end("ok");
  });
  const source = await startServer((req, res) => {
    res.writeHead(302, { location: `${target.origin}/final` });
    res.end();
  });

  try {
    const safeFetch = createRedirectSafeFetch(source.origin, fetch);
    const res = await safeFetch(`${source.origin}/start`, {
      headers: { "X-Custom": "keep-me" },
    });
    assert.equal(res.status, 200);
    assert.equal(targetHeaders["x-custom"], "keep-me");
  } finally {
    await source.close();
    await target.close();
  }
});

test("an explicit redirect mode is never overridden", async () => {
  let targetHit = false;
  const target = await startServer((req, res) => {
    targetHit = true;
    res.writeHead(200);
    res.end();
  });
  const source = await startServer((req, res) => {
    res.writeHead(302, { location: `${target.origin}/final` });
    res.end();
  });

  try {
    const safeFetch = createRedirectSafeFetch(source.origin, fetch);
    const res = await safeFetch(`${source.origin}/start`, {
      headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
      redirect: "manual",
    });
    assert.equal(res.status, 302);
    assert.equal(targetHit, false);
  } finally {
    await source.close();
    await target.close();
  }
});

test("too many redirects fail instead of looping forever", async () => {
  const server = await startServer((req, res) => {
    res.writeHead(302, { location: "/loop" });
    res.end();
  });

  try {
    const safeFetch = createRedirectSafeFetch(server.origin, fetch, { maxRedirects: 2 });
    await assert.rejects(
      () =>
        safeFetch(`${server.origin}/loop`, {
          headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
        }),
      /Too many redirects/,
    );
  } finally {
    await server.close();
  }
});

test("opaque (browser) redirects fail closed instead of leaking credentials", async () => {
  // Browsers report `redirect: "manual"` as an opaque redirect: type
  // "opaqueredirect" with status 0. The status check must not short-circuit
  // before this is detected.
  const opaqueRedirect = { status: 0, type: "opaqueredirect", headers: new Headers() };
  const safeFetch = createRedirectSafeFetch("http://a.test", async () => opaqueRedirect);

  await assert.rejects(
    () =>
      safeFetch("http://a.test/start", {
        headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
      }),
    (err) => {
      assert.ok(err instanceof SandboxApiException);
      assert.match(err.message, /cannot be inspected/);
      return true;
    },
  );
});

test("a status-0 response without an inspectable target fails closed", async () => {
  const zeroStatus = { status: 0, type: "default", headers: new Headers() };
  const safeFetch = createRedirectSafeFetch("http://a.test", async () => zeroStatus);

  await assert.rejects(
    () =>
      safeFetch("http://a.test/start", {
        headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
      }),
    (err) => err instanceof SandboxApiException,
  );
});

test("requests without protected headers pass opaque responses through", async () => {
  const opaqueRedirect = { status: 0, type: "opaqueredirect", headers: new Headers() };
  const safeFetch = createRedirectSafeFetch("http://a.test", async () => opaqueRedirect);

  const res = await safeFetch("http://a.test/start", { headers: { "X-Custom": "v" } });
  assert.equal(res, opaqueRedirect);
});

test("file uploads never follow redirects and surface the 3xx", async () => {
  let targetHit = false;
  const target = await startServer((req, res) => {
    targetHit = true;
    res.writeHead(200, { "content-type": "application/json" });
    res.end("{}");
  });
  const source = await startServer((req, res) => {
    res.writeHead(302, { location: `${target.origin}/files/upload` });
    res.end();
  });

  try {
    const opts = {
      baseUrl: source.origin,
      headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
      fetch,
    };
    const adapter = new FilesystemAdapter(createExecdClient(opts), opts);

    await assert.rejects(
      () => adapter.writeFiles([{ path: "/tmp/a.txt", data: "hello" }]),
      (err) => {
        assert.ok(err instanceof SandboxApiException);
        assert.equal(err.statusCode, 302);
        return true;
      },
    );
    assert.equal(targetHit, false);
  } finally {
    await source.close();
    await target.close();
  }
});

test("DefaultAdapterFactory strips protected headers on cross-origin data-plane redirects", async () => {
  let targetHeaders;
  const target = await startServer((req, res) => {
    targetHeaders = req.headers;
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok");
  });
  const source = await startServer((req, res) => {
    res.writeHead(302, { location: `${target.origin}/ping` });
    res.end();
  });

  try {
    const connectionConfig = new ConnectionConfig({ domain: "unused.test" });
    connectionConfig._fetch = fetch;
    connectionConfig._sseFetch = fetch;

    const execd = new DefaultAdapterFactory().createExecdStack({
      connectionConfig,
      execdBaseUrl: source.origin,
      endpointHeaders: { "OPENSANDBOX-EGRESS-AUTH": "egress-secret" },
    });

    assert.equal(await execd.health.ping(), true);
    assert.equal(targetHeaders["opensandbox-egress-auth"], undefined);
  } finally {
    await source.close();
    await target.close();
  }
});
