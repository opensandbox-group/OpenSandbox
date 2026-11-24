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

test("createSnapshot sends the requested mode and maps it back", async () => {
  let captured;
  const client = {
    async POST(path, options) {
      captured = { path, body: options.body };
      return {
        data: {
          id: "snap-1",
          sandboxId: "sbx-1",
          name: "workspace-state",
          mode: "rootfsOnly",
          status: { state: "Creating" },
          createdAt: "2026-03-14T12:00:00Z",
        },
        response: new Response(null, { status: 202 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  const snapshot = await adapter.createSnapshot("sbx-1", {
    name: "workspace-state",
    mode: "rootfsOnly",
  });

  assert.deepEqual(captured, {
    path: "/sandboxes/{sandboxId}/snapshots",
    body: { name: "workspace-state", mode: "rootfsOnly" },
  });
  assert.equal(snapshot.mode, "rootfsOnly");
  assert.ok(snapshot.createdAt instanceof Date);
});

test("createSnapshot keeps an unset mode off the wire and reads legacy responses as full", async () => {
  let captured;
  const client = {
    async POST(path, options) {
      captured = { path, body: options.body };
      return {
        data: {
          id: "snap-1",
          sandboxId: "sbx-1",
          name: "baseline",
          status: { state: "Creating" },
          createdAt: "2026-03-14T12:00:00Z",
        },
        response: new Response(null, { status: 202 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  const snapshot = await adapter.createSnapshot("sbx-1", { name: "baseline" });

  assert.deepEqual(captured.body, { name: "baseline" });
  assert.equal(snapshot.mode, "full");
});

test("getSnapshot maps a rootfsOnly response mode", async () => {
  const client = {
    async GET() {
      return {
        data: {
          id: "snap-1",
          sandboxId: "sbx-1",
          name: "workspace-state",
          mode: "rootfsOnly",
          status: { state: "Ready" },
          createdAt: "2026-03-14T12:00:00Z",
        },
        response: new Response(null, { status: 200 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  const snapshot = await adapter.getSnapshot("snap-1");

  assert.equal(snapshot.mode, "rootfsOnly");
});
