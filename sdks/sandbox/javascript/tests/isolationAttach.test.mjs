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

describe("attach", () => {
  it("populates full info when execd returns all fields", async () => {
    const calls = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (method === "GET" && urlStr.endsWith("/v1/isolated/session/sess-full")) {
        calls.push({ method, url: urlStr });
        return new Response(
          JSON.stringify({
            status: "active",
            created_at: "2026-01-02T03:04:05Z",
            last_run_at: "2026-01-02T03:05:06Z",
            idle_remaining_seconds: 30,
            profile: "strict",
            workspace: { path: "/workspace", mode: "rw" },
            extra_writable: ["/tmp", "/var/tmp"],
            binds: [{ source: "/host/a", dest: "/sbx/a", readonly: true }],
            share_net: false,
            env_passthrough: { mode: "allow", keys: ["PATH", "HOME"] },
            uid: 1000,
            gid: 2000,
            uid_mode: "userns",
            idle_timeout_seconds: 300,
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const session = await adapter.attach("sess-full");

    assert.strictEqual(calls.length, 1);
    assert.strictEqual(calls[0].method, "GET");
    assert.ok(calls[0].url.endsWith("/v1/isolated/session/sess-full"));

    assert.strictEqual(session.sessionId, "sess-full");
    const info = session.info;
    assert.strictEqual(info.session_id, "sess-full");
    assert.strictEqual(info.created_at, "2026-01-02T03:04:05Z");
    assert.strictEqual(info.profile, "strict");
    assert.deepStrictEqual(info.workspace, { path: "/workspace", mode: "rw" });
    assert.deepStrictEqual(info.extra_writable, ["/tmp", "/var/tmp"]);
    assert.strictEqual(info.binds?.length, 1);
    assert.deepStrictEqual(info.binds[0], {
      source: "/host/a",
      dest: "/sbx/a",
      readonly: true,
    });
    assert.strictEqual(info.share_net, false);
    assert.deepStrictEqual(info.env_passthrough, {
      mode: "allow",
      keys: ["PATH", "HOME"],
    });
    assert.strictEqual(info.uid, 1000);
    assert.strictEqual(info.gid, 2000);
    assert.strictEqual(info.uid_mode, "userns");
    assert.strictEqual(info.idle_timeout_seconds, 300);
  });

  it("tolerates missing creation params when execd is older", async () => {
    const calls = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-old")
      ) {
        calls.push({ method, url: urlStr });
        // Older execd: only runtime status fields, no creation-parameter echoes.
        return new Response(
          JSON.stringify({
            status: "active",
            created_at: "2026-01-02T03:04:05Z",
            last_run_at: null,
            idle_remaining_seconds: calls.length === 1 ? null : 7,
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (
        method === "DELETE" &&
        urlStr.endsWith("/v1/isolated/session/sess-old")
      ) {
        calls.push({ method, url: urlStr });
        return new Response(null, { status: 204 });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const session = await adapter.attach("sess-old");

    const info = session.info;
    assert.strictEqual(info.session_id, "sess-old");
    assert.strictEqual(info.created_at, "2026-01-02T03:04:05Z");
    assert.strictEqual(info.profile, undefined);
    assert.strictEqual(info.workspace, undefined);
    assert.strictEqual(info.extra_writable, undefined);
    assert.strictEqual(info.binds, undefined);
    assert.strictEqual(info.share_net, undefined);
    assert.strictEqual(info.env_passthrough, undefined);
    assert.strictEqual(info.uid, undefined);
    assert.strictEqual(info.gid, undefined);
    assert.strictEqual(info.uid_mode, undefined);
    assert.strictEqual(info.idle_timeout_seconds, undefined);

    // get() and delete() must still work — they only need sessionId.
    const state = await session.get();
    assert.strictEqual(state.status, "active");
    assert.strictEqual(state.idle_remaining_seconds, 7);

    await session.delete();

    assert.deepStrictEqual(
      calls.map((c) => c.method),
      ["GET", "GET", "DELETE"],
    );
  });

  it("propagates not-found when session missing", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/missing-sess")
      ) {
        return new Response(
          JSON.stringify({
            code: "SESSION_NOT_FOUND",
            message: "isolated session not found",
          }),
          { status: 404, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("wrong endpoint", { status: 500 });
    };
    const adapter = createAdapter(mockFn);

    await assert.rejects(
      () => adapter.attach("missing-sess"),
      (err) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /404/);
        assert.match(err.message, /\/v1\/isolated\/session\/missing-sess/);
        return true;
      },
    );
  });

  it("rejects blank sessionId", async () => {
    const adapter = createAdapter(async () => new Response("", { status: 200 }));
    await assert.rejects(() => adapter.attach(""), /sessionId cannot be empty/);
    await assert.rejects(() => adapter.attach("   "), /sessionId cannot be empty/);
  });
});
