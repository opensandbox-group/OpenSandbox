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
