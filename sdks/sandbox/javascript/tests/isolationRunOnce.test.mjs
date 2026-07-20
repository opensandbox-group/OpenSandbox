import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { IsolatedSessionsAdapter } from "../dist/internal.js";

function createAdapter(mockFetch) {
  return new IsolatedSessionsAdapter({
    baseUrl: "http://localhost:8080",
    fetch: mockFetch,
    sseFetch: mockFetch,
    headers: { "X-Test": "1" },
  });
}

function mockFetchForSession(sessionId, opts = {}) {
  const calls = [];
  const mockFn = async (url, init) => {
    const urlStr = typeof url === "string" ? url : url.toString();
    const method = init?.method ?? "GET";

    if (method === "POST" && urlStr.includes("/v1/isolated/session") && !urlStr.includes("/run")) {
      calls.push("create");
      return new Response(
        JSON.stringify({ session_id: sessionId, created_at: "2026-01-01T00:00:00Z" }),
        { status: 201, headers: { "content-type": "application/json" } },
      );
    }

    if (method === "POST" && urlStr.includes("/run")) {
      calls.push("run");
      if (opts.runFails) {
        return new Response("internal error", { status: 500 });
      }
      const body = 'data: {"type":"complete"}\n\n';
      return new Response(body, {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      });
    }

    if (method === "DELETE") {
      calls.push("delete");
      if (opts.deleteFails) {
        return new Response("gone", { status: 500 });
      }
      return new Response(null, { status: 204 });
    }

    return new Response("not found", { status: 404 });
  };

  return { mockFn, calls };
}

describe("runOnce", () => {
  it("calls create → run → delete in sequence", async () => {
    const { mockFn, calls } = mockFetchForSession("sess-1");
    const adapter = createAdapter(mockFn);

    const result = await adapter.runOnce("echo hello", "/workspace");

    assert.deepStrictEqual(calls, ["create", "run", "delete"]);
    assert.ok(result);
  });

  it("deletes session even when run fails", async () => {
    const { mockFn, calls } = mockFetchForSession("sess-2", { runFails: true });
    const adapter = createAdapter(mockFn);

    await assert.rejects(() => adapter.runOnce("bad", "/workspace"));
    assert.deepStrictEqual(calls, ["create", "run", "delete"]);
  });

  it("tolerates delete failure", async () => {
    const { mockFn, calls } = mockFetchForSession("sess-3", { deleteFails: true });
    const adapter = createAdapter(mockFn);

    const result = await adapter.runOnce("echo ok", "/workspace");

    assert.deepStrictEqual(calls, ["create", "run", "delete"]);
    assert.ok(result);
  });
});

describe("list", () => {
  it("returns the sessions array from the response", async () => {
    const calls = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (method === "GET" && urlStr.includes("/v1/isolated/sessions")) {
        calls.push("list");
        return new Response(
          JSON.stringify({
            sessions: [
              {
                session_id: "sess-a",
                status: "active",
                created_at: "2026-01-01T00:00:00Z",
                last_run_at: "2026-01-01T00:01:00Z",
                idle_remaining_seconds: 42,
              },
            ],
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const sessions = await adapter.list();

    assert.deepStrictEqual(calls, ["list"]);
    assert.strictEqual(sessions.length, 1);
    assert.strictEqual(sessions[0].session_id, "sess-a");
    assert.strictEqual(sessions[0].status, "active");
    assert.strictEqual(sessions[0].idle_remaining_seconds, 42);
  });

  it("returns an empty array when sessions is absent", async () => {
    const mockFn = async () =>
      new Response(JSON.stringify({}), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    const adapter = createAdapter(mockFn);

    const sessions = await adapter.list();

    assert.deepStrictEqual(sessions, []);
  });
});

describe("withSession", () => {
  it("calls create, runs callback, then deletes", async () => {
    const { mockFn, calls } = mockFetchForSession("sess-ws");
    const adapter = createAdapter(mockFn);

    const result = await adapter.withSession(
      { workspace: { path: "/workspace" } },
      async (session) => {
        assert.strictEqual(session.sessionId, "sess-ws");
        return "done";
      },
    );

    assert.strictEqual(result, "done");
    assert.deepStrictEqual(calls, ["create", "delete"]);
  });

  it("deletes session when callback throws", async () => {
    const { mockFn, calls } = mockFetchForSession("sess-ws-err");
    const adapter = createAdapter(mockFn);

    await assert.rejects(
      () => adapter.withSession({ workspace: { path: "/workspace" } }, async () => {
        throw new Error("callback error");
      }),
      { message: "callback error" },
    );

    assert.deepStrictEqual(calls, ["create", "delete"]);
  });
});

describe("create request body", () => {
  function mockCapturingCreate(captured) {
    return async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (method === "POST" && urlStr.includes("/v1/isolated/session") && !urlStr.includes("/run")) {
        captured.body = JSON.parse(init.body);
        return new Response(
          JSON.stringify({ session_id: "sess-body", created_at: "2026-01-01T00:00:00Z" }),
          { status: 201, headers: { "content-type": "application/json" } },
        );
      }
      if (method === "DELETE") return new Response(null, { status: 204 });
      return new Response("not found", { status: 404 });
    };
  }

  it("serializes binds and uid_mode into the create body", async () => {
    const captured = {};
    const adapter = createAdapter(mockCapturingCreate(captured));

    await adapter.withSession(
      {
        workspace: { path: "/workspace", mode: "rw" },
        binds: [
          { source: "/data/in", dest: "/mnt/in", readonly: true },
          { source: "/data/out" },
        ],
        uid_mode: "userns",
      },
      async () => "ok",
    );

    assert.deepStrictEqual(captured.body.binds, [
      { source: "/data/in", dest: "/mnt/in", readonly: true },
      { source: "/data/out" },
    ]);
    assert.strictEqual(captured.body.uid_mode, "userns");
  });

  it("omits binds and uid_mode when unset", async () => {
    const captured = {};
    const adapter = createAdapter(mockCapturingCreate(captured));

    await adapter.withSession({ workspace: { path: "/workspace" } }, async () => "ok");

    assert.ok(!("binds" in captured.body));
    assert.ok(!("uid_mode" in captured.body));
  });
});
