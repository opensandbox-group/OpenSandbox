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
