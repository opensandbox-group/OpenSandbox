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

describe("_get", () => {
  it("populates full state when execd returns all fields", async () => {
    const calls = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-full")
      ) {
        calls.push({ method, url: urlStr });
        return new Response(
          JSON.stringify({
            status: "active",
            created_at: "2026-01-02T03:04:05Z",
            last_run_at: "2026-01-02T03:05:06Z",
            idle_remaining_seconds: 42,
            profile: "balanced",
            workspace: { path: "/workspace", mode: "overlay" },
            extra_writable: ["/tmp"],
            binds: [{ source: "/host/x", dest: "/sbx/x", readonly: false }],
            share_net: true,
            env_passthrough: { mode: "deny", keys: ["SECRET"] },
            uid: 1001,
            gid: 2001,
            uid_mode: "setpriv",
            idle_timeout_seconds: 600,
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const state = await adapter._get("sess-full");

    assert.strictEqual(calls.length, 1);
    assert.strictEqual(calls[0].method, "GET");
    assert.ok(calls[0].url.endsWith("/v1/isolated/session/sess-full"));

    assert.strictEqual(state.status, "active");
    assert.strictEqual(state.created_at, "2026-01-02T03:04:05Z");
    assert.strictEqual(state.last_run_at, "2026-01-02T03:05:06Z");
    assert.strictEqual(state.idle_remaining_seconds, 42);
    assert.strictEqual(state.profile, "balanced");
    assert.deepStrictEqual(state.workspace, {
      path: "/workspace",
      mode: "overlay",
    });
    assert.deepStrictEqual(state.extra_writable, ["/tmp"]);
    assert.strictEqual(state.binds?.length, 1);
    assert.deepStrictEqual(state.binds[0], {
      source: "/host/x",
      dest: "/sbx/x",
      readonly: false,
    });
    assert.strictEqual(state.share_net, true);
    assert.deepStrictEqual(state.env_passthrough, {
      mode: "deny",
      keys: ["SECRET"],
    });
    assert.strictEqual(state.uid, 1001);
    assert.strictEqual(state.gid, 2001);
    assert.strictEqual(state.uid_mode, "setpriv");
    assert.strictEqual(state.idle_timeout_seconds, 600);
  });

  it("tolerates missing echo fields when execd is older", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-old")
      ) {
        return new Response(
          JSON.stringify({
            status: "active",
            created_at: "2026-01-02T03:04:05Z",
            last_run_at: null,
            idle_remaining_seconds: null,
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const state = await adapter._get("sess-old");

    assert.strictEqual(state.status, "active");
    assert.strictEqual(state.created_at, "2026-01-02T03:04:05Z");
    assert.strictEqual(state.profile, undefined);
    assert.strictEqual(state.workspace, undefined);
    assert.strictEqual(state.extra_writable, undefined);
    assert.strictEqual(state.binds, undefined);
    assert.strictEqual(state.share_net, undefined);
    assert.strictEqual(state.env_passthrough, undefined);
    assert.strictEqual(state.uid, undefined);
    assert.strictEqual(state.gid, undefined);
    assert.strictEqual(state.uid_mode, undefined);
    assert.strictEqual(state.idle_timeout_seconds, undefined);
  });

  it("preserves idle_timeout_seconds: 0 (idle GC disabled)", async () => {
    // Regression guard: 0 is a meaningful value (idle GC disabled, long-window
    // recovery). It must NOT be treated as falsy/missing.
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-zero")
      ) {
        return new Response(
          JSON.stringify({
            status: "active",
            created_at: "2026-01-02T03:04:05Z",
            idle_remaining_seconds: null,
            idle_timeout_seconds: 0,
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const state = await adapter._get("sess-zero");

    assert.strictEqual(state.status, "active");
    assert.strictEqual(state.idle_timeout_seconds, 0);
    assert.notStrictEqual(state.idle_timeout_seconds, undefined);
  });
});
