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

function createAdapter() {
  const requests = [];
  const rawRequests = [];
  const client = {
    async POST(path, options) {
      assert.equal(path, "/sandboxes");
      rawRequests.push(options.body);
      requests.push(JSON.parse(JSON.stringify(options.body)));
      return {
        data: {
          id: "sandbox-1",
          status: { state: "Running" },
          entrypoint: ["sleep", "infinity"],
          createdAt: "2026-08-20T00:00:00Z",
          expiresAt: null,
        },
        response: new Response(null, { status: 202 }),
      };
    },
  };
  return { adapter: new SandboxesAdapter(client), rawRequests, requests };
}

test("createSandbox forwards lifecycle hooks in the standard request body", async () => {
  const { adapter, requests } = createAdapter();
  const lifecycle = {
    preStart: { command: ["/opt/hooks/restore.sh"], timeoutSeconds: 60 },
    periodic: [
      {
        name: "checkpoint",
        schedule: "*/5 * * * *",
        command: ["/opt/hooks/checkpoint.sh"],
        timeoutSeconds: 30,
      },
    ],
  };
  const expectedLifecycle = structuredClone(lifecycle);

  await adapter.createSandbox({ lifecycle });

  assert.deepEqual(requests[0].lifecycle, expectedLifecycle);
  assert.deepEqual(lifecycle, expectedLifecycle);
});

test("createSandbox omits lifecycle when it is not configured", async () => {
  const { adapter, rawRequests, requests } = createAdapter();

  await adapter.createSandbox({ lifecycle: undefined });

  assert.equal(Object.hasOwn(requests[0], "lifecycle"), false);
  assert.equal(Object.hasOwn(rawRequests[0], "lifecycle"), false);
});

test("createSandbox normalizes an empty lifecycle to absent", async () => {
  for (const lifecycle of [
    {},
    { periodic: [] },
    { preStart: undefined, periodic: [] },
    { preStart: null, periodic: null },
  ]) {
    const { adapter, rawRequests, requests } = createAdapter();
    const request = { lifecycle };
    const lifecycleSnapshot = structuredClone(lifecycle);

    await adapter.createSandbox(request);

    assert.equal(Object.hasOwn(requests[0], "lifecycle"), false);
    assert.equal(Object.hasOwn(rawRequests[0], "lifecycle"), false);
    assert.equal(Object.hasOwn(request, "lifecycle"), true);
    assert.deepEqual(request.lifecycle, lifecycleSnapshot);
  }
});

test("createSandbox omits empty periodic when preStart is configured", async () => {
  const { adapter, rawRequests, requests } = createAdapter();
  const lifecycle = { preStart: { command: ["true"] }, periodic: [] };
  const request = { lifecycle };
  const lifecycleSnapshot = structuredClone(lifecycle);

  await adapter.createSandbox(request);

  assert.deepEqual(requests[0].lifecycle, { preStart: { command: ["true"] } });
  assert.equal(Object.hasOwn(rawRequests[0].lifecycle, "periodic"), false);
  assert.equal(Object.hasOwn(request, "lifecycle"), true);
  assert.deepEqual(lifecycle, lifecycleSnapshot);
});

test("createSandbox preserves invalid periodic values for server validation", async () => {
  const { adapter, requests } = createAdapter();

  await adapter.createSandbox({ lifecycle: { periodic: { schedule: "@hourly" } } });

  assert.deepEqual(requests[0].lifecycle, { periodic: { schedule: "@hourly" } });
});

test("createSandbox preserves future lifecycle hooks", async () => {
  const { adapter, requests } = createAdapter();
  const lifecycle = { preTerminate: { command: ["/opt/hooks/flush.sh"] } };
  const expectedLifecycle = structuredClone(lifecycle);

  await adapter.createSandbox({ lifecycle });

  assert.deepEqual(requests[0].lifecycle, expectedLifecycle);
  assert.deepEqual(lifecycle, expectedLifecycle);
});

test("deleteSandbox forwards AbortSignal to the transport", async () => {
  const controller = new AbortController();
  let deleteOptions;
  const adapter = new SandboxesAdapter({
    async DELETE(path, options) {
      assert.equal(path, "/sandboxes/{sandboxId}");
      deleteOptions = options;
      return { response: new Response(null, { status: 204 }) };
    },
  });

  await adapter.deleteSandbox("sandbox-1", controller.signal);

  assert.equal(deleteOptions.params.path.sandboxId, "sandbox-1");
  assert.equal(deleteOptions.signal, controller.signal);
});
