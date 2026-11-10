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
  IsolatedFilesystemAdapter,
  createExecdClient,
  createRedirectSafeFetch,
  hasProtectedHeaders,
  isSameOrigin,
  stripCredentialHeaders,
  stripProtectedHeaders,
  _resetClientIpCacheForTest,
  _setClientIpForTest,
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

  const creds = new Headers({
    authorization: "Bearer t",
    cookie: "session=1",
    "proxy-authorization": "Basic p",
    "x-keep": "v",
  });
  stripCredentialHeaders(creds);
  assert.equal(creds.has("authorization"), false);
  assert.equal(creds.has("cookie"), false);
  assert.equal(creds.has("proxy-authorization"), false);
  assert.equal(creds.get("x-keep"), "v");

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

test("cross-origin redirect strips standard credential headers too", async () => {
  const calls = [];
  const baseFetch = async (input, init) => {
    const request = new Request(input, init);
    calls.push(Object.fromEntries(request.headers));
    if (calls.length === 1) {
      return new Response(null, { status: 302, headers: { location: "http://other.test/final" } });
    }
    return new Response("ok", { status: 200 });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch);

  await safeFetch("http://a.test/start", {
    headers: {
      "OPEN-SANDBOX-API-KEY": "tenant-secret",
      Authorization: "Bearer user-token",
      Cookie: "session=abc",
      "X-Custom": "keep-me",
    },
  });

  assert.equal(calls.length, 2);
  // The configured origin still receives everything.
  assert.equal(calls[0]["authorization"], "Bearer user-token");
  assert.equal(calls[0]["cookie"], "session=abc");
  // The cross-origin hop drops standard credentials (mirroring native fetch).
  assert.equal(calls[1]["authorization"], undefined);
  assert.equal(calls[1]["cookie"], undefined);
  assert.equal(calls[1]["open-sandbox-api-key"], undefined);
  // Non-credential headers survive.
  assert.equal(calls[1]["x-custom"], "keep-me");
});

test("307/308 redirects replay a Request body instead of surfacing the 3xx", async () => {
  const calls = [];
  const baseFetch = async (input, init) => {
    const request = new Request(input, init);
    calls.push({ url: request.url, method: request.method, body: await request.text() });
    if (calls.length === 1) {
      return new Response(null, { status: 308, headers: { location: "http://other.test/final" } });
    }
    return new Response("ok", { status: 200 });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch);

  // openapi-fetch passes a Request object carrying a JSON body.
  const payload = JSON.stringify({ name: "sandbox" });
  const request = new Request("http://a.test/start", {
    method: "POST",
    headers: { "content-type": "application/json", "OPEN-SANDBOX-API-KEY": "tenant-secret" },
    body: payload,
  });

  const res = await safeFetch(request);
  assert.equal(res.status, 200);
  assert.equal(calls.length, 2);
  assert.equal(calls[0].method, "POST");
  assert.equal(calls[1].method, "POST");
  assert.equal(calls[1].url, "http://other.test/final");
  assert.equal(calls[1].body, payload);
});

test("the request signal is carried across redirected hops", async () => {
  const controller = new AbortController();
  const signals = [];
  const baseFetch = async (input, init) => {
    const request = new Request(input, init);
    signals.push(request.signal);
    if (signals.length === 1) {
      return new Response(null, { status: 302, headers: { location: "http://other.test/final" } });
    }
    // Simulate a stalled target that only settles on abort.
    await Promise.race([
      new Promise((resolve) =>
        request.signal.addEventListener("abort", resolve, { once: true }),
      ),
      new Promise((resolve) => setTimeout(resolve, 2000)),
    ]);
    if (request.signal.aborted) throw request.signal.reason;
    return new Response("late", { status: 200 });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch);

  const request = new Request("http://a.test/start", {
    headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
    signal: controller.signal,
  });
  const promise = safeFetch(request);
  setTimeout(() => controller.abort(new Error("cancelled")), 10);

  await assert.rejects(promise, /cancelled/);
  assert.equal(signals.length, 2);
  assert.equal(signals[1].aborted, true);
});

test("Request-level fetch options survive the rebuild of every guarded hop", async () => {
  const inits = [];
  const baseFetch = async (input, init) => {
    inits.push(init);
    if (inits.length === 1) {
      return new Response(null, { status: 307, headers: { location: "http://other.test/final" } });
    }
    return new Response("ok", { status: 200 });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch);

  const request = new Request("http://a.test/start", {
    method: "POST",
    body: "payload",
    headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
    credentials: "include",
    cache: "no-store",
    referrerPolicy: "no-referrer",
  });

  const res = await safeFetch(request);
  assert.equal(res.status, 200);
  assert.equal(inits.length, 2);
  for (const init of inits) {
    // Options that only live on the Request input must be copied onto the
    // rebuilt (url, init) hop instead of falling back to platform defaults.
    assert.equal(init.credentials, "include");
    assert.equal(init.cache, "no-store");
    assert.equal(init.referrerPolicy, "no-referrer");
    // ...while the loop keeps ownership of the redirect mode.
    assert.equal(init.redirect, "manual");
  }
});

test("the overall redirect deadline bounds the first URL-input hop", async () => {
  const signals = [];
  const baseFetch = async (input, init) => {
    signals.push(init?.signal);
    await new Promise((resolve) => {
      init?.signal?.addEventListener("abort", resolve, { once: true });
      setTimeout(resolve, 1000);
    });
    if (init?.signal?.aborted) throw init.signal.reason;
    return new Response("late", { status: 200 });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch, {
    overallTimeoutMs: 20,
  });

  await assert.rejects(
    safeFetch("http://a.test/start", { headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" } }),
    /Redirect chain timed out/,
  );
  // A string/URL input used to take the header-less first hop without any
  // signal, so the overall deadline only started at the first redirect.
  assert.equal(signals.length, 1);
  assert.equal(signals[0].aborted, true);
});

test("a non-finite or negative overall deadline disables it", async () => {
  // ConnectionConfig treats requestTimeoutSeconds <= 0 or Infinity as "no
  // timeout"; the redirect guard must not turn that into an immediate abort.
  for (const overallTimeoutMs of [Infinity, -1000]) {
    const baseFetch = async (input, init) => {
      await new Promise((resolve) => setTimeout(resolve, 50));
      if (init?.signal?.aborted) throw init.signal.reason;
      return new Response("ok", { status: 200 });
    };
    const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch, { overallTimeoutMs });

    const res = await safeFetch("http://a.test/start", {
      headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
    });
    assert.equal(await res.text(), "ok", `overallTimeoutMs=${overallTimeoutMs}`);
  }
});

test("303 and POST 301/302 convert to GET and drop entity headers", async () => {
  const calls = [];
  const baseFetch = async (input, init) => {
    const request = new Request(input, init);
    calls.push({
      method: request.method,
      headers: Object.fromEntries(request.headers),
      body: await request.text(),
    });
    if (calls.length === 1) {
      return new Response(null, { status: 303, headers: { location: "http://other.test/final" } });
    }
    return new Response("ok", { status: 200 });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch);

  const request = new Request("http://a.test/start", {
    method: "POST",
    headers: { "content-type": "application/json", "OPEN-SANDBOX-API-KEY": "tenant-secret" },
    body: JSON.stringify({ a: 1 }),
  });

  const res = await safeFetch(request);
  assert.equal(res.status, 200);
  assert.equal(calls[1].method, "GET");
  assert.equal(calls[1].body, "");
  assert.equal(calls[1].headers["content-type"], undefined);
});

test("a Request carrying redirect: manual is not followed", async () => {
  const calls = [];
  const baseFetch = async (input, init) => {
    const request = new Request(input, init);
    calls.push(request.url);
    return new Response(null, { status: 302, headers: { location: "http://other.test/final" } });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch);

  const request = new Request("http://a.test/start", {
    method: "GET",
    headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
    redirect: "manual",
  });

  const res = await safeFetch(request);
  assert.equal(res.status, 302);
  // Only the original request was issued; the redirect was not chased.
  assert.equal(calls.length, 1);
});

test("non-HTTP(S) Location values are surfaced, not followed", async () => {
  const calls = [];
  const baseFetch = async (input, init) => {
    calls.push(new Request(input, init).url);
    return new Response(null, {
      status: 302,
      headers: { location: "data:application/json,%7B%7D" },
    });
  };
  const safeFetch = createRedirectSafeFetch("http://a.test", baseFetch);

  const res = await safeFetch("http://a.test/start", {
    headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
  });
  assert.equal(res.status, 302);
  assert.equal(calls.length, 1);
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
  // Disable the client-IP guard so nothing needs protection on this request.
  _resetClientIpCacheForTest();
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

test("cross-origin hops drop the client-IP header added by the transport", async () => {
  let sourceHeaders;
  let targetHeaders;
  const target = await startServer((req, res) => {
    targetHeaders = req.headers;
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok");
  });
  const source = await startServer((req, res) => {
    sourceHeaders = req.headers;
    res.writeHead(302, { location: `${target.origin}/ping` });
    res.end();
  });

  _setClientIpForTest("10.9.8.7");
  const connectionConfig = new ConnectionConfig({ domain: "unused.test" }).withTransportIfMissing();
  try {
    const execd = new DefaultAdapterFactory().createExecdStack({
      connectionConfig,
      execdBaseUrl: source.origin,
      endpointHeaders: { "OPENSANDBOX-EGRESS-AUTH": "egress-secret" },
    });

    assert.equal(await execd.health.ping(), true);
    // The configured origin still sees the SDK host's IP...
    assert.equal(sourceHeaders["open-sandbox-client-ip"], "10.9.8.7");
    // ...but the redirect target does not.
    assert.equal(targetHeaders["open-sandbox-client-ip"], undefined);
  } finally {
    await connectionConfig.closeTransport();
    _resetClientIpCacheForTest();
    await source.close();
    await target.close();
  }
});

test("guards a transport-injected client-IP without caller protected headers", async () => {
  let sourceHeaders;
  let targetHeaders;
  const target = await startServer((req, res) => {
    targetHeaders = req.headers;
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok");
  });
  const source = await startServer((req, res) => {
    sourceHeaders = req.headers;
    res.writeHead(302, { location: `${target.origin}/ping` });
    res.end();
  });

  _setClientIpForTest("10.9.8.7");
  const connectionConfig = new ConnectionConfig({ domain: "unused.test" }).withTransportIfMissing();
  try {
    // No protected headers from the caller: only the transport-injected
    // client-IP makes this request need the guard.
    const execd = new DefaultAdapterFactory().createExecdStack({
      connectionConfig,
      execdBaseUrl: source.origin,
    });

    assert.equal(await execd.health.ping(), true);
    assert.equal(sourceHeaders["open-sandbox-client-ip"], "10.9.8.7");
    assert.equal(targetHeaders["open-sandbox-client-ip"], undefined);
  } finally {
    await connectionConfig.closeTransport();
    _resetClientIpCacheForTest();
    await source.close();
    await target.close();
  }
});

test("same-origin redirect chains keep the client-IP on every hop", async () => {
  const hops = [];
  const server = await startServer((req, res) => {
    hops.push({ url: req.url, clientIp: req.headers["open-sandbox-client-ip"] });
    if (req.url === "/ping") {
      res.writeHead(302, { location: "/ping2" });
      res.end();
      return;
    }
    if (req.url === "/ping2") {
      res.writeHead(302, { location: "/ping3" });
      res.end();
      return;
    }
    res.writeHead(200, { "content-type": "text/plain" });
    res.end("ok");
  });

  _setClientIpForTest("10.9.8.7");
  const connectionConfig = new ConnectionConfig({ domain: "unused.test" }).withTransportIfMissing();
  try {
    const execd = new DefaultAdapterFactory().createExecdStack({
      connectionConfig,
      execdBaseUrl: server.origin,
      endpointHeaders: { "OPENSANDBOX-EGRESS-AUTH": "egress-secret" },
    });

    assert.equal(await execd.health.ping(), true);
    assert.equal(hops.length, 3);
    for (const hop of hops) {
      assert.equal(hop.clientIp, "10.9.8.7");
    }
  } finally {
    await connectionConfig.closeTransport();
    _resetClientIpCacheForTest();
    await server.close();
  }
});

test("isolated file uploads never follow redirects and surface the 3xx", async () => {
  let targetHit = false;
  const target = await startServer((req, res) => {
    targetHit = true;
    res.writeHead(200, { "content-type": "application/json" });
    res.end("{}");
  });
  const source = await startServer((req, res) => {
    res.writeHead(302, {
      location: `${target.origin}/v1/isolated/session/s1/files/upload`,
    });
    res.end();
  });

  try {
    const opts = {
      baseUrl: source.origin,
      sessionId: "s1",
      headers: { "OPEN-SANDBOX-API-KEY": "tenant-secret" },
      fetch,
    };
    const adapter = new IsolatedFilesystemAdapter(createExecdClient(opts), opts);

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
