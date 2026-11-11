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

test("listSnapshots forwards the exact name filter", async () => {
  let captured;
  const client = {
    async GET(path, options) {
      captured = { path, query: options.params.query };
      return {
        data: {
          items: [],
          pagination: {
            page: 1,
            pageSize: 20,
            totalItems: 0,
            totalPages: 0,
            hasNextPage: false,
          },
        },
        response: new Response(null, { status: 200 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  await adapter.listSnapshots({ name: "toolchain:node@rev-1" });

  assert.deepEqual(captured, {
    path: "/snapshots",
    query: { name: "toolchain:node@rev-1" },
  });
});
