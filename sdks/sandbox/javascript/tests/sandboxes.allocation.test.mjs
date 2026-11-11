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

import { SandboxesAdapter } from "../dist/internal.js";

function sandbox(overrides = {}) {
  return {
    id: "sandbox-1",
    status: { state: "Running" },
    entrypoint: ["sleep", "infinity"],
    createdAt: "2026-07-10T00:00:00Z",
    expiresAt: null,
    ...overrides,
  };
}

test("getSandbox and listSandboxes expose a confirmed Pool allocation", async () => {
  const allocation = {
    mode: "pool",
    poolRef: "default-pool",
    state: "allocated",
  };
  const client = {
    async GET(path) {
      if (path === "/sandboxes/{sandboxId}") {
        return {
          data: sandbox({ allocation }),
          response: new Response(null, { status: 200 }),
        };
      }
      assert.equal(path, "/sandboxes");
      return {
        data: {
          items: [sandbox({ allocation })],
          pagination: {
            page: 1,
            pageSize: 20,
            totalItems: 1,
            totalPages: 1,
            hasNextPage: false,
          },
        },
        response: new Response(null, { status: 200 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  const result = await adapter.getSandbox("sandbox-1");
  const listed = await adapter.listSandboxes();

  assert.deepEqual(result.allocation, allocation);
  assert.deepEqual(listed.items[0].allocation, allocation);
});

test("getSandbox and listSandboxes leave allocation absent for non-Pool sandboxes", async () => {
  const client = {
    async GET(path) {
      if (path === "/sandboxes/{sandboxId}") {
        return {
          data: sandbox(),
          response: new Response(null, { status: 200 }),
        };
      }
      assert.equal(path, "/sandboxes");
      return {
        data: {
          items: [sandbox()],
          pagination: {
            page: 1,
            pageSize: 20,
            totalItems: 1,
            totalPages: 1,
            hasNextPage: false,
          },
        },
        response: new Response(null, { status: 200 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  const result = await adapter.getSandbox("sandbox-1");
  const listed = await adapter.listSandboxes();

  assert.equal(Object.hasOwn(result, "allocation"), false);
  assert.equal(result.allocation, undefined);
  assert.equal(Object.hasOwn(listed.items[0], "allocation"), false);
  assert.equal(listed.items[0].allocation, undefined);
});
