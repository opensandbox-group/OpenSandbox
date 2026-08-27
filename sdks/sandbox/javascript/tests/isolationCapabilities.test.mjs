import assert from "node:assert/strict";
import test from "node:test";

import { IsolatedSessionsAdapter } from "../dist/internal.js";

test("capabilities exposes uid mode availability", async () => {
  const adapter = new IsolatedSessionsAdapter({
    baseUrl: "http://localhost:8080",
    fetch: async () =>
      new Response(
        JSON.stringify({
          available: true,
          isolator: "bwrap",
          setpriv_available: false,
          userns_available: true,
          commit_supported: false,
          diff_supported: false,
        }),
        { status: 200, headers: { "content-type": "application/json" } },
      ),
    headers: {},
  });

  const capabilities = await adapter.capabilities();

  assert.equal(capabilities.setpriv_available, false);
  assert.equal(capabilities.userns_available, true);
});

test("capabilities defaults missing uid mode availability to false", async () => {
  const adapter = new IsolatedSessionsAdapter({
    baseUrl: "http://localhost:8080",
    fetch: async () =>
      new Response(
        JSON.stringify({
          available: true,
          commit_supported: false,
          diff_supported: false,
        }),
        { status: 200, headers: { "content-type": "application/json" } },
      ),
    headers: {},
  });

  const capabilities = await adapter.capabilities();

  assert.equal(capabilities.setpriv_available, false);
  assert.equal(capabilities.userns_available, false);
});
