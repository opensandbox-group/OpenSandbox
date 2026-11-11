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

import { NetworkPolicyAdapter } from "../dist/internal.js";

function createClient() {
  const calls = [];
  const client = {
    async GET(path, options) {
      calls.push({ method: "GET", path, options });
      return {
        data: {
          status: "ok",
          mode: "deny_all",
          policy: {
            defaultAction: "deny",
            egress: [{ action: "allow", target: "pypi.org" }],
          },
        },
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
    async DELETE(path, options) {
      calls.push({ method: "DELETE", path, options });
      return {
        data: { status: "ok", policy: { defaultAction: "deny", egress: [] } },
        response: new Response(null, { status: 200 }),
      };
    },
  };
  return { client, calls };
}

test("NetworkPolicyAdapter reads the policy payload from the control plane", async () => {
  const { client, calls } = createClient();
  const adapter = new NetworkPolicyAdapter(client, "sbx-1");

  const policy = await adapter.getPolicy();

  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].path, "/sandboxes/{sandboxId}/networkpolicy");
  assert.deepEqual(calls[0].options.params.path, { sandboxId: "sbx-1" });
  assert.deepEqual(policy, {
    defaultAction: "deny",
    egress: [{ action: "allow", target: "pypi.org" }],
  });
});

test("NetworkPolicyAdapter patches rules against the control plane", async () => {
  const { client, calls } = createClient();
  const adapter = new NetworkPolicyAdapter(client, "sbx-1");
  const rules = [{ action: "allow", target: "www.github.com" }];

  await adapter.patchRules(rules);

  assert.equal(calls[0].method, "PATCH");
  assert.equal(calls[0].path, "/sandboxes/{sandboxId}/networkpolicy");
  assert.deepEqual(calls[0].options.params.path, { sandboxId: "sbx-1" });
  assert.deepEqual(calls[0].options.body, rules);
});

test("NetworkPolicyAdapter deletes rules by target against the control plane", async () => {
  const { client, calls } = createClient();
  const adapter = new NetworkPolicyAdapter(client, "sbx-1");

  await adapter.deleteRules(["www.github.com"]);

  assert.equal(calls[0].method, "DELETE");
  assert.equal(calls[0].path, "/sandboxes/{sandboxId}/networkpolicy");
  assert.deepEqual(calls[0].options.params.path, { sandboxId: "sbx-1" });
  assert.deepEqual(calls[0].options.body, ["www.github.com"]);
});
