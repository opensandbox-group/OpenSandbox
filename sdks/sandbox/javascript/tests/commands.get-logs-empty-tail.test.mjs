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

import { CommandsAdapter, createExecdClient } from "../dist/internal.js";

function createTransportAdapter({ body = "", cursor = "42" } = {}) {
  const client = createExecdClient({
    baseUrl: "http://127.0.0.1:8080",
    async fetch(request) {
      assert.equal(new URL(request.url).pathname, "/command/cmd-1/logs");
      assert.equal(new URL(request.url).searchParams.get("cursor"), "42");
      return new Response(body, {
        status: 200,
        headers: {
          "content-type": "text/plain",
          "EXECD-COMMANDS-TAIL-CURSOR": cursor,
        },
      });
    },
  });

  return new CommandsAdapter(client, {
    baseUrl: "http://127.0.0.1:8080",
  });
}

function createNullBodyAdapter({ cursor = "42" } = {}) {
  return new CommandsAdapter(
    {
      async GET() {
        return {
          data: null,
          error: undefined,
          response: new Response(null, {
            status: 200,
            headers: {
              "content-type": "text/plain",
              "EXECD-COMMANDS-TAIL-CURSOR": cursor,
            },
          }),
        };
      },
    },
    {
      baseUrl: "http://127.0.0.1:8080",
    },
  );
}

test("CommandsAdapter.getBackgroundCommandLogs accepts an empty transport body", async () => {
  const adapter = createTransportAdapter({ body: "", cursor: "42" });

  const logs = await adapter.getBackgroundCommandLogs("cmd-1", 42);

  assert.equal(logs.content, "");
  assert.equal(logs.cursor, 42);
});

test("CommandsAdapter.getBackgroundCommandLogs accepts a null parsed body on 200 responses", async () => {
  const adapter = createNullBodyAdapter({ cursor: "42" });

  const logs = await adapter.getBackgroundCommandLogs("cmd-1", 42);

  assert.equal(logs.content, "");
  assert.equal(logs.cursor, 42);
});

test("CommandsAdapter.getBackgroundCommandLogs rejects a negative cursor before transport", async () => {
  const adapter = new CommandsAdapter(
    {
      async GET() {
        throw new Error("transport should not be called");
      },
    },
    { baseUrl: "http://unused.invalid" },
  );

  await assert.rejects(
    () => adapter.getBackgroundCommandLogs("cmd-1", -1),
    /cursor cannot be negative/,
  );
});
