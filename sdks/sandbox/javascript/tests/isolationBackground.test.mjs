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

const BACKGROUND_RUN_PAYLOAD = {
  session_id: "sess-1",
  run_id: "run-1",
  started_at: "2026-01-02T03:04:05Z",
};

describe("runBackground", () => {
  it("posts background:true without timeout_seconds and parses the 202 handle", async () => {
    const captured = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (method === "POST" && urlStr.endsWith("/v1/isolated/session/sess-1/run")) {
        captured.push({ url: urlStr, headers: init.headers, body: JSON.parse(init.body) });
        return new Response(JSON.stringify(BACKGROUND_RUN_PAYLOAD), {
          status: 202,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const run = await adapter._runBackground(
      "sess-1",
      "echo hi",
      { envs: { A: "b" }, timeout_seconds: 30 },
    );

    assert.strictEqual(captured.length, 1);
    assert.ok(captured[0].url.endsWith("/v1/isolated/session/sess-1/run"));
    assert.strictEqual(captured[0].body.code, "echo hi");
    assert.strictEqual(captured[0].body.background, true);
    assert.deepStrictEqual(captured[0].body.envs, { A: "b" });
    assert.ok(!("timeout_seconds" in captured[0].body));

    assert.strictEqual(run.session_id, "sess-1");
    assert.strictEqual(run.run_id, "run-1");
    assert.strictEqual(run.started_at, "2026-01-02T03:04:05Z");
  });

  it("omits envs when unset", async () => {
    let body = null;
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (method === "POST" && urlStr.endsWith("/v1/isolated/session/sess-1/run")) {
        body = JSON.parse(init.body);
        return new Response(JSON.stringify(BACKGROUND_RUN_PAYLOAD), { status: 202 });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const run = await adapter._runBackground("sess-1", "echo hi");

    assert.strictEqual(body.code, "echo hi");
    assert.strictEqual(body.background, true);
    assert.ok(!("envs" in body));
    assert.strictEqual(run.run_id, "run-1");
  });

  it("propagates not-found (404) from the run endpoint", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (method === "POST" && urlStr.endsWith("/v1/isolated/session/sess-1/run")) {
        return new Response(
          JSON.stringify({ code: "SESSION_NOT_FOUND", message: "isolated session not found" }),
          { status: 404, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("wrong endpoint", { status: 500 });
    };
    const adapter = createAdapter(mockFn);

    await assert.rejects(
      () => adapter._runBackground("sess-1", "echo hi"),
      (err) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /404/);
        assert.match(err.message, /\/v1\/isolated\/session\/sess-1\/run/);
        return true;
      },
    );
  });
});

describe("getRunStatus", () => {
  it("parses a finished status with exit code and timestamps", async () => {
    const calls = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-1/runs/run-1")
      ) {
        calls.push({ method, url: urlStr });
        return new Response(
          JSON.stringify({
            session_id: "sess-1",
            run_id: "run-1",
            running: false,
            exit_code: 7,
            started_at: "2026-01-02T03:04:05Z",
            finished_at: "2026-01-02T03:04:09Z",
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const status = await adapter._getRunStatus("sess-1", "run-1");

    assert.strictEqual(calls.length, 1);
    assert.strictEqual(calls[0].method, "GET");
    assert.ok(calls[0].url.endsWith("/v1/isolated/session/sess-1/runs/run-1"));

    assert.strictEqual(status.running, false);
    assert.strictEqual(status.exit_code, 7);
    assert.strictEqual(status.started_at, "2026-01-02T03:04:05Z");
    assert.strictEqual(status.finished_at, "2026-01-02T03:04:09Z");
  });

  it("parses a running status without exit code", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-1/runs/run-1")
      ) {
        return new Response(
          JSON.stringify({
            session_id: "sess-1",
            run_id: "run-1",
            running: true,
            started_at: "2026-01-02T03:04:05Z",
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const status = await adapter._getRunStatus("sess-1", "run-1");

    assert.strictEqual(status.running, true);
    assert.strictEqual(status.exit_code, undefined);
    assert.strictEqual(status.finished_at, undefined);
  });

  it("propagates not-found (404) from the status endpoint", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-1/runs/run-missing")
      ) {
        return new Response(
          JSON.stringify({ code: "RUN_NOT_FOUND", message: "run not found" }),
          { status: 404, headers: { "content-type": "application/json" } },
        );
      }
      return new Response("wrong endpoint", { status: 500 });
    };
    const adapter = createAdapter(mockFn);

    await assert.rejects(
      () => adapter._getRunStatus("sess-1", "run-missing"),
      (err) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /404/);
        assert.match(err.message, /\/v1\/isolated\/session\/sess-1\/runs\/run-missing/);
        return true;
      },
    );
  });
});

describe("getRunLogs", () => {
  it("sends the cursor query and uses the tail cursor header", async () => {
    const calls = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.includes("/v1/isolated/session/sess-1/runs/run-1/logs")
      ) {
        calls.push({ url: urlStr });
        return new Response("line1\nline2\n", {
          status: 200,
          headers: {
            "content-type": "text/plain",
            "EXECD-ISOLATED-TAIL-CURSOR": "12",
          },
        });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const logs = await adapter._getRunLogs("sess-1", "run-1", 4);

    assert.strictEqual(calls.length, 1);
    const url = new URL(calls[0].url);
    assert.strictEqual(url.searchParams.get("cursor"), "4");
    assert.strictEqual(logs.text, "line1\nline2\n");
    assert.strictEqual(logs.cursor, 12);
  });

  it("omits the cursor query when cursor is 0", async () => {
    const calls = [];
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.includes("/v1/isolated/session/sess-1/runs/run-1/logs")
      ) {
        calls.push({ url: urlStr });
        return new Response("hello", {
          status: 200,
          headers: { "content-type": "text/plain" },
        });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const logs = await adapter._getRunLogs("sess-1", "run-1");

    assert.strictEqual(calls.length, 1);
    assert.ok(!new URL(calls[0].url).searchParams.has("cursor"));
    assert.strictEqual(logs.text, "hello");
  });

  it("falls back to requested cursor plus byte length when the header is absent", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.includes("/v1/isolated/session/sess-1/runs/run-1/logs")
      ) {
        return new Response("hello", {
          status: 200,
          headers: { "content-type": "text/plain" },
        });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const logs = await adapter._getRunLogs("sess-1", "run-1", 2);

    assert.strictEqual(logs.text, "hello");
    assert.strictEqual(logs.cursor, 2 + 5);
  });

  it("counts bytes (not characters) when the header is absent", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.includes("/v1/isolated/session/sess-1/runs/run-1/logs")
      ) {
        return new Response("h\u00e9llo", {
          status: 200,
          headers: { "content-type": "text/plain" },
        });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const logs = await adapter._getRunLogs("sess-1", "run-1");

    // "héllo" is 5 characters but 6 UTF-8 bytes.
    assert.strictEqual(logs.cursor, 6);
  });

  it("propagates not-found (404) from the logs endpoint", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (
        method === "GET" &&
        urlStr.includes("/v1/isolated/session/sess-1/runs/run-missing/logs")
      ) {
        return new Response("run gone", {
          status: 404,
          headers: { "content-type": "text/plain" },
        });
      }
      return new Response("wrong endpoint", { status: 500 });
    };
    const adapter = createAdapter(mockFn);

    await assert.rejects(
      () => adapter._getRunLogs("sess-1", "run-missing"),
      (err) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /404/);
        assert.match(err.message, /\/v1\/isolated\/session\/sess-1\/runs\/run-missing\/logs/);
        return true;
      },
    );
  });
});

describe("handle wiring", () => {
  it("delegates runBackground/getRunStatus/getRunLogs through the session handle", async () => {
    const mockFn = async (url, init) => {
      const urlStr = typeof url === "string" ? url : url.toString();
      const method = init?.method ?? "GET";
      if (method === "GET" && urlStr.endsWith("/v1/isolated/session/sess-1")) {
        return new Response(
          JSON.stringify({
            status: "active",
            created_at: "2026-01-02T03:04:05Z",
          }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (method === "POST" && urlStr.endsWith("/v1/isolated/session/sess-1/run")) {
        return new Response(JSON.stringify(BACKGROUND_RUN_PAYLOAD), { status: 202 });
      }
      if (
        method === "GET" &&
        urlStr.endsWith("/v1/isolated/session/sess-1/runs/run-1")
      ) {
        return new Response(
          JSON.stringify({ session_id: "sess-1", run_id: "run-1", running: false, exit_code: 0 }),
          { status: 200, headers: { "content-type": "application/json" } },
        );
      }
      if (
        method === "GET" &&
        urlStr.includes("/v1/isolated/session/sess-1/runs/run-1/logs")
      ) {
        return new Response("bg-output\n", {
          status: 200,
          headers: { "content-type": "text/plain", "EXECD-ISOLATED-TAIL-CURSOR": "10" },
        });
      }
      return new Response("not found", { status: 404 });
    };
    const adapter = createAdapter(mockFn);

    const session = await adapter.attach("sess-1");

    const run = await session.runBackground("echo hi");
    assert.strictEqual(run.session_id, "sess-1");
    assert.strictEqual(run.run_id, "run-1");

    const status = await session.getRunStatus("run-1");
    assert.strictEqual(status.running, false);
    assert.strictEqual(status.exit_code, 0);

    const logs = await session.getRunLogs("run-1");
    assert.strictEqual(logs.text, "bg-output\n");
    assert.strictEqual(logs.cursor, 10);

    const tail = await session.getRunLogs("run-1", logs.cursor);
    assert.strictEqual(tail.text, "bg-output\n");
  });
});

describe("background validation", () => {
  it("rejects blank sessionId/code/runId and negative cursor", async () => {
    const adapter = createAdapter(async () => new Response("", { status: 200 }));

    await assert.rejects(() => adapter._runBackground("", "echo hi"), /sessionId cannot be empty/);
    await assert.rejects(() => adapter._runBackground("sess-1", "  "), /code cannot be empty/);
    await assert.rejects(() => adapter._getRunStatus("", "run-1"), /sessionId cannot be empty/);
    await assert.rejects(() => adapter._getRunStatus("sess-1", ""), /runId cannot be empty/);
    await assert.rejects(() => adapter._getRunLogs("sess-1", "run-1", -1), /cursor cannot be negative/);
  });
});
