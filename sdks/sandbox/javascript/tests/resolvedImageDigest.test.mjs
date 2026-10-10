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

for (const value of [undefined, null, `sha256:${"a".repeat(64)}`]) {
  test(`resolved image digest survives create/get/list: ${value}`, async () => {
    const payload = {
      id: "sandbox-1",
      image: { uri: "python:3.11" },
      status: { state: "Running" },
      entrypoint: ["python"],
      createdAt: "2026-10-09T00:00:00Z",
      ...(value === undefined ? {} : { resolvedImageDigest: value }),
    };
    const adapter = new SandboxesAdapter({
      async POST() {
        return { data: payload, response: new Response(null, { status: 202 }) };
      },
      async GET(path) {
        return {
          data: path === "/sandboxes" ? { items: [payload] } : payload,
          response: new Response(null, { status: 200 }),
        };
      },
    });
    assert.equal((await adapter.createSandbox({})).resolvedImageDigest, value);
    assert.equal((await adapter.getSandbox("sandbox-1")).resolvedImageDigest, value);
    assert.equal((await adapter.listSandboxes()).items[0].resolvedImageDigest, value);
  });
}
