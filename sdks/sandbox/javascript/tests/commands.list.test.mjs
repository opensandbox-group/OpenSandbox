// Copyright 2026 Alibaba Group Holding Ltd.
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

import { CommandsAdapter } from "../dist/internal.js";

function createAdapter(data) {
  return new CommandsAdapter(
    { GET: async () => ({ data, response: new Response(null, { status: 200 }) }) },
    { baseUrl: "http://127.0.0.1:8080" },
  );
}

function runningSummary(overrides = {}) {
  return {
    session: "running-session",
    running: true,
    background: true,
    started_at: "2026-07-22T01:02:03Z",
    ...overrides,
  };
}

function terminalSummary(overrides = {}) {
  return {
    session: "terminal-session",
    running: false,
    background: false,
    started_at: "2026-07-22T01:01:03Z",
    finished_at: "2026-07-22T01:01:04Z",
    exit_code: 0,
    ...overrides,
  };
}

function page(commands, pagination = { limit: 1 }) {
  return { commands, pagination };
}

test("CommandsAdapter.listCommands normalizes blank query cursors and maps mixed summaries", async () => {
  let request;
  const client = {
    GET: async (path, options) => {
      request = { path, options };
      return {
        data: {
          commands: [
            {
              session: "running-session",
              running: true,
              background: true,
              started_at: "2026-07-22T01:02:03Z",
            },
            {
              session: "terminal-session",
              running: false,
              background: false,
              started_at: "2026-07-22T01:01:03Z",
              finished_at: "2026-07-22T01:01:04Z",
              exit_code: null,
              error: "killed",
            },
          ],
          pagination: { limit: 2, nextCursor: "x" },
        },
        response: new Response(null, { status: 200 }),
      };
    },
  };
  const adapter = new CommandsAdapter(client, { baseUrl: "http://127.0.0.1:8080" });

  let page;
  for (const cursor of ["", " \t "]) {
    page = await adapter.listCommands({ running: false, limit: 2, cursor });
    assert.deepEqual(request, {
      path: "/command",
      options: { params: { query: { running: false, limit: 2 } } },
    });
  }

  assert.equal(page.commands[0].running, true);
  assert.equal(page.commands[1].running, false);
  assert.equal(page.commands[1].exitCode, null);
  assert.equal(page.pagination.nextCursor, "x");
});

test("CommandsAdapter.listCommands preserves a nonblank opaque query cursor", async () => {
  const cursor = "  opaque +/=  ";
  let request;
  const client = {
    GET: async (path, options) => {
      request = { path, options };
      return {
        data: { commands: [], pagination: { limit: 1, nextCursor: cursor } },
        response: new Response(null, { status: 200 }),
      };
    },
  };
  const adapter = new CommandsAdapter(client, { baseUrl: "http://127.0.0.1:8080" });

  const page = await adapter.listCommands({ cursor });

  assert.equal(request.options.params.query.cursor, cursor);
  assert.equal(page.pagination.nextCursor, cursor);
});

test("CommandsAdapter.listCommands omits absent query values and final nextCursor", async () => {
  let request;
  const client = {
    GET: async (path, options) => {
      request = { path, options };
      return {
        data: { commands: [], pagination: { limit: 50 } },
        response: new Response(null, { status: 200 }),
      };
    },
  };
  const adapter = new CommandsAdapter(client, { baseUrl: "http://127.0.0.1:8080" });

  const page = await adapter.listCommands();

  assert.deepEqual(request, { path: "/command", options: { params: { query: {} } } });
  assert.equal(page.pagination.nextCursor, undefined);
});

test("CommandsAdapter.listCommands rejects blank nextCursor values", async () => {
  for (const nextCursor of ["", " \t "]) {
    await assert.rejects(
      createAdapter(page([], { limit: 50, nextCursor })).listCommands(),
      /Invalid nextCursor/,
    );
  }
});

test("CommandsAdapter.listCommands rejects a terminal summary without exit_code", async () => {
  const client = {
    GET: async () => ({
      data: {
        commands: [
          {
            session: "terminal-session",
            running: false,
            background: false,
            started_at: "2026-07-22T01:01:03Z",
            finished_at: "2026-07-22T01:01:04Z",
          },
        ],
        pagination: { limit: 1 },
      },
      response: new Response(null, { status: 200 }),
    }),
  };
  const adapter = new CommandsAdapter(client, { baseUrl: "http://127.0.0.1:8080" });

  await assert.rejects(adapter.listCommands(), /Invalid terminal command summary/);
});

test("CommandsAdapter.listCommands strictly validates command summary branches", async () => {
  const invalidSummaries = [
    runningSummary({ running: "true" }),
    runningSummary({ finished_at: "2026-07-22T01:02:04Z" }),
    runningSummary({ unexpected: true }),
    terminalSummary({ error: null }),
    terminalSummary({ exit_code: undefined }),
    terminalSummary({ exit_code: "0" }),
    terminalSummary({ exit_code: 0.5 }),
    terminalSummary({ exit_code: 2147483648 }),
    terminalSummary({ started_at: "2026-07-22" }),
    terminalSummary({ finished_at: "2026-07-22T01:01:04+01:99" }),
  ];

  for (const summary of invalidSummaries) {
    await assert.rejects(createAdapter(page([summary])).listCommands());
  }
});

test("CommandsAdapter.listCommands accepts RFC3339 offsets and nullable int32 exit codes", async () => {
  const result = await createAdapter(
    page([
      terminalSummary({
        started_at: "2026-07-22T01:01:03.123456+05:30",
        finished_at: "2016-12-31t23:59:59z",
        exit_code: null,
      }),
    ]),
  ).listCommands();

  assert.equal(result.commands[0].exitCode, null);
  assert.equal(result.commands[0].startedAt.toISOString(), "2026-07-21T19:31:03.123Z");
  assert.equal(result.commands[0].finishedAt.toISOString(), "2016-12-31T23:59:59.000Z");
});

test("CommandsAdapter.listCommands preserves RFC3339 years before 0100", async () => {
  const result = await createAdapter(
    page([
      runningSummary({ started_at: "0001-01-01T00:00:00Z" }),
      terminalSummary({
        started_at: "0000-02-29T00:00:00Z",
        finished_at: "0000-02-29T00:00:01Z",
      }),
    ], { limit: 2 }),
  ).listCommands();

  assert.equal(result.commands[0].startedAt.getUTCFullYear(), 1);
  assert.equal(result.commands[1].startedAt.getUTCFullYear(), 0);
  assert.equal(result.commands[1].startedAt.getUTCMonth(), 1);
  assert.equal(result.commands[1].startedAt.getUTCDate(), 29);
});

test("CommandsAdapter.listCommands rejects leap-second timestamps", async () => {
  await assert.rejects(
    createAdapter(page([runningSummary({ started_at: "2016-12-31T23:59:60Z" })])).listCommands(),
    /Invalid started_at/,
  );
});

test("CommandsAdapter.listCommands strictly validates pages while allowing extensions", async () => {
  const invalidPages = [
    { commands: [], pagination: { limit: true } },
    { commands: [], pagination: { limit: 0 } },
    { commands: [], pagination: { limit: 101 } },
    { commands: [], pagination: { limit: 1, nextCursor: null } },
    { commands: [], pagination: { limit: 1, nextCursor: undefined } },
    { commands: [], pagination: { limit: 1, nextCursor: 1 } },
  ];

  for (const invalidPage of invalidPages) {
    await assert.rejects(createAdapter(invalidPage).listCommands());
  }

  const result = await createAdapter({
    commands: [],
    pagination: { limit: 1, page_extension: true },
    top_level_extension: true,
  }).listCommands();
  assert.equal(result.pagination.limit, 1);
});
