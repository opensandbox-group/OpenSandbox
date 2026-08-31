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

import {
  detectOutboundIp,
  selectClientIp,
  getClientIp,
  ensureClientIpReady,
  withClientIp,
  _setClientIpForTest,
  _restartDetectionForTest,
  _resetClientIpCacheForTest,
} from "../dist/internal.js";

const HEADER = "OPEN-SANDBOX-CLIENT-IP";

function headersOf(result) {
  // Normalize to a Headers instance from either the returned init or Request input.
  if (result.input instanceof Request) return new Headers(result.input.headers);
  return new Headers(result.init?.headers);
}

test("withClientIp sets the header for a string input", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("10.9.8.7");
  const out = withClientIp("http://x/y", { headers: { "X-Foo": "bar" } });
  const h = headersOf(out);
  assert.equal(h.get(HEADER), "10.9.8.7");
  assert.equal(h.get("X-Foo"), "bar"); // existing header preserved
});

test("withClientIp preserves headers already on a Request input", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("10.9.8.7");
  const req = new Request("http://x/y", {
    method: "POST",
    headers: { "X-Demo-Label": "js", "OPEN-SANDBOX-API-KEY": "secret" },
  });
  const out = withClientIp(req, undefined);
  const h = headersOf(out);
  assert.equal(h.get(HEADER), "10.9.8.7");
  assert.equal(h.get("X-Demo-Label"), "js"); // must NOT be dropped
  assert.equal(h.get("OPEN-SANDBOX-API-KEY"), "secret"); // must NOT be dropped
  assert.ok(out.input instanceof Request);
  assert.equal(out.input.method, "POST"); // method preserved
});

test("withClientIp merges headers when input is a Request AND init has headers", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("10.9.8.7");
  const req = new Request("http://x/y", {
    method: "POST",
    headers: { "X-Req-Only": "r", "OPEN-SANDBOX-API-KEY": "secret" },
  });
  const out = withClientIp(req, { headers: { "X-Init": "i" } });
  // fetch(request, init) lets init.headers replace the request's headers, so
  // BOTH the returned Request and the returned init must carry the full merged
  // set — whichever the runtime reads, nothing is dropped.
  const fromInput = new Headers(out.input instanceof Request ? out.input.headers : undefined);
  const fromInit = new Headers(out.init?.headers);
  for (const h of [fromInput, fromInit]) {
    assert.equal(h.get(HEADER), "10.9.8.7");
    assert.equal(h.get("X-Req-Only"), "r");
    assert.equal(h.get("OPEN-SANDBOX-API-KEY"), "secret");
    assert.equal(h.get("X-Init"), "i");
  }
});

test("withClientIp keeps merged headers when client IP is already on the Request", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("10.9.8.7");
  // User already set the client IP on the Request; init has an unrelated header.
  const req = new Request("http://x/y", {
    headers: { "OPEN-SANDBOX-CLIENT-IP": "192.168.0.9", "X-Req-Only": "r" },
  });
  const out = withClientIp(req, { headers: { "X-Init": "i" } });
  const fromInput = new Headers(out.input instanceof Request ? out.input.headers : undefined);
  const fromInit = new Headers(out.init?.headers);
  for (const h of [fromInput, fromInit]) {
    assert.equal(h.get(HEADER), "192.168.0.9"); // user value kept, not overwritten
    assert.equal(h.get("X-Req-Only"), "r"); // Request-only header not dropped
    assert.equal(h.get("X-Init"), "i"); // init header not dropped
  }
});

test("withClientIp keeps merged headers when client IP is already in init", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("10.9.8.7");
  // Client IP present in init; a request-only header must survive the merge.
  const req = new Request("http://x/y", { headers: { "X-Req-Only": "r" } });
  const out = withClientIp(req, {
    headers: { "open-sandbox-client-ip": "192.168.0.9", "X-Init": "i" },
  });
  const fromInput = new Headers(out.input instanceof Request ? out.input.headers : undefined);
  const fromInit = new Headers(out.init?.headers);
  for (const h of [fromInput, fromInit]) {
    assert.equal(h.get(HEADER), "192.168.0.9");
    assert.equal(h.get("X-Req-Only"), "r");
    assert.equal(h.get("X-Init"), "i");
  }
});

test("withClientIp does not overwrite a user-provided value", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("10.9.8.7");
  const out = withClientIp("http://x/y", {
    headers: { "open-sandbox-client-ip": "192.168.0.9" },
  });
  assert.equal(headersOf(out).get(HEADER), "192.168.0.9");
});

test("withClientIp is a no-op when the IP is unavailable", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("");
  const out = withClientIp("http://x/y", { headers: { "X-Foo": "bar" } });
  assert.equal(headersOf(out).get(HEADER), null);
  assert.equal(headersOf(out).get("X-Foo"), "bar");
});

test("getClientIp returns the cached value", (t) => {
  t.after(_resetClientIpCacheForTest);
  _setClientIpForTest("172.16.5.4");
  assert.equal(getClientIp(), "172.16.5.4");
});

test("ensureClientIpReady awaits detection so the first request carries the header", async (t) => {
  t.after(_resetClientIpCacheForTest);
  // Start a fresh real probe (do NOT pre-seed the cache), then await it. This
  // exercises the actual async detection -> ensureClientIpReady -> header path.
  _restartDetectionForTest();
  assert.equal(getClientIp(), "", "cache must be empty before detection resolves");
  await ensureClientIpReady();
  const ip = getClientIp();
  if (ip === "") {
    // Make the skip visible in CI rather than passing silently.
    t.diagnostic("no outbound IP detected (network-less env); skipping header assertion");
    return;
  }
  const out = withClientIp("http://x/y", undefined);
  assert.equal(headersOf(out).get(HEADER), ip);
});

test("selectClientIp prefers a named NIC and skips virtual ones", () => {
  const ip = selectClientIp([
    { name: "docker0", ips: ["172.17.0.1"] }, // virtual, skip
    { name: "utun3", ips: ["10.8.0.3"] }, // VPN, skip
    { name: "en0", ips: ["10.1.1.1"] }, // main NIC
    { name: "eth1", ips: ["192.168.5.5"] },
  ]);
  assert.equal(ip, "10.1.1.1");
});

test("selectClientIp falls back to a private IPv4 when no named NIC", () => {
  const ip = selectClientIp([
    { name: "docker0", ips: ["172.17.0.1"] }, // virtual, skip
    { name: "custom9", ips: ["8.8.4.4"] }, // public, lower preference
    { name: "wlan9", ips: ["10.0.0.50"] }, // private, preferred
  ]);
  assert.equal(ip, "10.0.0.50");
});

test("selectClientIp skips loopback and link-local", () => {
  const ip = selectClientIp([
    { name: "eth0", ips: ["127.0.0.1", "169.254.1.1", "10.1.2.3"] },
  ]);
  assert.equal(ip, "10.1.2.3");
});

test("selectClientIp returns empty when nothing usable", () => {
  const ip = selectClientIp([
    { name: "docker0", ips: ["172.17.0.1"] },
    { name: "eth0", ips: ["169.254.1.1"] },
  ]);
  assert.equal(ip, "");
});

test("detectOutboundIp returns a valid IP or empty string", async (t) => {
  t.after(_resetClientIpCacheForTest);
  _resetClientIpCacheForTest();
  const ip = await detectOutboundIp();
  if (ip === "") {
    // Make the skip visible in CI rather than passing silently.
    t.diagnostic("detectOutboundIp returned empty (network-less env); skipping format assertions");
    return;
  }
  // Octets constrained to 0-255 so an obviously invalid address would fail.
  const octet = "(25[0-5]|2[0-4]\\d|1?\\d?\\d)";
  assert.match(ip, new RegExp(`^${octet}(\\.${octet}){3}$`));
  assert.ok(!ip.startsWith("127."), `unexpected loopback IP: ${ip}`);
  assert.notEqual(ip, "0.0.0.0");
});
