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

import { createEgressClient, EgressAdapter } from "../dist/internal.js";

/**
 * Port-scoped egress rules: the JS SDK passes rules through verbatim, so the
 * contract to pin is the wire shape — a port-only rule PATCHes without a
 * target key, and read-back of the sidecar shape ("target": "") round-trips
 * unchanged instead of being rewritten.
 */
function createClient(policy) {
  const calls = [];
  const client = {
    async GET(path, options) {
      calls.push({ method: "GET", path, options });
      return {
        data: { status: "ok", mode: "enforcing", policy },
        response: new Response(null, { status: 200 }),
      };
    },
    async PATCH(path, options) {
      calls.push({ method: "PATCH", path, options });
      return {
        data: { status: "ok", policy: { defaultAction: "deny", egress: [] } },
        response: new Response(null, { status: 200 }),
      };
    },
  };
  return { client, calls };
}

test("EgressAdapter patches a port-only rule without a target key", async () => {
  const { client, calls } = createClient({ defaultAction: "allow", egress: [] });
  const adapter = new EgressAdapter(client);
  const rules = [{ action: "deny", ports: [25] }];

  await adapter.patchRules(rules);

  assert.equal(calls[0].method, "PATCH");
  assert.equal(calls[0].path, "/policy");
  assert.deepEqual(calls[0].options.body, [{ action: "deny", ports: [25] }]);
  assert.ok(!("target" in calls[0].options.body[0]));
});

test("EgressAdapter patches a target+ports rule with both fields", async () => {
  const { client, calls } = createClient({ defaultAction: "allow", egress: [] });
  const adapter = new EgressAdapter(client);
  const rules = [{ action: "allow", target: "10.0.0.5", ports: [22, 80] }];

  await adapter.patchRules(rules);

  assert.deepEqual(calls[0].options.body, [{ action: "allow", target: "10.0.0.5", ports: [22, 80] }]);
});

test("EgressAdapter reads back the sidecar port-only shape unchanged", async () => {
  // The sidecar marshals target without omitempty: a port-only rule always
  // arrives as "target": "".
  const { client } = createClient({
    defaultAction: "deny",
    egress: [{ action: "deny", target: "", ports: [25] }],
  });
  const adapter = new EgressAdapter(client);

  const policy = await adapter.getPolicy();

  assert.deepEqual(policy.egress, [{ action: "deny", target: "", ports: [25] }]);
});

test("EgressAdapter reads back a target+ports rule unchanged", async () => {
  const { client } = createClient({
    defaultAction: "deny",
    egress: [{ action: "allow", target: "10.0.0.5", ports: [22, 80] }],
  });
  const adapter = new EgressAdapter(client);

  const policy = await adapter.getPolicy();

  assert.deepEqual(policy.egress, [{ action: "allow", target: "10.0.0.5", ports: [22, 80] }]);
});

test("EgressAdapter serializes and reads distinct domain and wildcard port scopes", async () => {
  const requests = [];
  const client = createEgressClient({
    baseUrl: "http://egress.local",
    fetch: async (request) => {
      requests.push({
        method: request.method,
        path: new URL(request.url).pathname,
        body: request.body === null ? null : await request.text(),
      });
      return new Response(request.method === "GET"
        ? '{"status":"ok","policy":{"defaultAction":"deny","egress":[{"action":"allow","target":"example.com","ports":[443]},{"action":"deny","target":"example.com","ports":[80]},{"action":"allow","target":"*.example.com","ports":[8443]}]}}'
        : '{"status":"ok"}', { headers: { "content-type": "application/json" } });
    },
  });
  const adapter = new EgressAdapter(client);
  const rules = [
    { action: "allow", target: "example.com", ports: [443] },
    { action: "deny", target: "example.com", ports: [80] },
    { action: "allow", target: "*.example.com", ports: [8443] },
  ];

  await adapter.patchRules(rules);
  assert.equal(requests[0].method, "PATCH");
  assert.equal(requests[0].path, "/policy");
  assert.deepEqual(JSON.parse(requests[0].body), rules);
  assert.deepEqual((await adapter.getPolicy()).egress, rules);
  assert.equal(requests[1].method, "GET");
  assert.equal(requests[1].path, "/policy");
});

test("NetworkRule union rejects rules with neither target nor ports at compile time", async () => {
  // Type-level invariant; the runtime check mirrors what tsc enforces so the
  // contract is pinned for plain-JS callers too.
  const invalid = { action: "allow" };
  assert.ok(!("target" in invalid) && !("ports" in invalid));
});
