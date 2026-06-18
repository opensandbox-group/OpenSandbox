// Copyright 2025 Alibaba Group Holding Ltd.
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

/**
 * Tests for PoolsAdapter – HTTP call behaviour, correct URL/method routing,
 * request body construction, error propagation.
 */

import { describe, it, expect, vi } from "vitest";
import { PoolsAdapter } from "../src/adapters/poolsAdapter.js";
import { SandboxApiException } from "../src/core/exceptions.js";
import type { PoolComponents } from "../src/api/lifecycle.js";

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

interface CapturedRequest {
  url: string;
  method: string;
  body?: unknown;
  headers: Record<string, string>;
}

function makeFetch(
  status: number,
  body: unknown,
  capture?: { requests: CapturedRequest[] }
): typeof fetch {
  return async (input, init) => {
    // openapi-fetch may pass a Request object as `input` with method/headers baked in
    let url: string;
    let method: string;
    let bodyStr: string | null = null;
    const headers: Record<string, string> = {};

    if (input instanceof Request) {
      url = input.url;
      method = (input.method ?? "GET").toUpperCase();
      bodyStr = await input.text().catch(() => null);
      input.headers.forEach((v, k) => { headers[k.toLowerCase()] = v; });
    } else {
      url = typeof input === "string" ? input : String(input);
      method = ((init?.method ?? "GET") as string).toUpperCase();
      if (init?.body) {
        try { bodyStr = init.body as string; } catch { /* ignore */ }
      }
      if (init?.headers) {
        const h = init.headers as Record<string, string>;
        for (const [k, v] of Object.entries(h)) {
          headers[k.toLowerCase()] = v;
        }
      }
    }

    let parsedBody: unknown;
    if (bodyStr) {
      try { parsedBody = JSON.parse(bodyStr); } catch { parsedBody = bodyStr; }
    }

    capture?.requests.push({ url, method, body: parsedBody, headers });
    return new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  };
}

function makePoolResponse(
  name = "test-pool",
  bufferMax = 3,
  poolMax = 10
): PoolComponents["schemas"]["PoolResponse"] {
  return {
    name,
    capacitySpec: { bufferMax, bufferMin: 1, poolMax, poolMin: 0 },
    status: { total: 2, allocated: 1, available: 1, revision: "rev-1" },
    createdAt: "2025-06-01T00:00:00.000Z",
  };
}

function makeAdapter(status: number, body: unknown, capture?: { requests: CapturedRequest[] }): PoolsAdapter {
  return new PoolsAdapter({
    baseUrl: "http://server.local/v1",
    apiKey: "key-123",
    fetch: makeFetch(status, body, capture) as unknown as typeof fetch,
  });
}

// ---------------------------------------------------------------------------
// createPool
// ---------------------------------------------------------------------------

describe("PoolsAdapter.createPool", () => {
  it("sends POST to /pools", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const adapter = makeAdapter(201, makePoolResponse("new-pool"), captured);

    await adapter.createPool({
      name: "new-pool",
      template: { spec: {} },
      capacitySpec: { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0 },
    });

    expect(captured.requests).toHaveLength(1);
    const req = captured.requests[0];
    expect(req.method).toBe("POST");
    expect(req.url).toContain("/pools");
    expect(req.url).not.toContain("/pools/");
  });

  it("sends correct request body", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const adapter = makeAdapter(201, makePoolResponse(), captured);

    await adapter.createPool({
      name: "ci-pool",
      template: { spec: { containers: [{ name: "sbx" }] } },
      capacitySpec: { bufferMax: 5, bufferMin: 2, poolMax: 20, poolMin: 1 },
    });

    const body = captured.requests[0].body as Record<string, unknown>;
    expect(body.name).toBe("ci-pool");
    const cap = body.capacitySpec as Record<string, number>;
    expect(cap.bufferMax).toBe(5);
    expect(cap.poolMax).toBe(20);
    expect(cap.poolMin).toBe(1);
  });

  it("returns mapped PoolInfo", async () => {
    const adapter = makeAdapter(201, makePoolResponse("created", 3, 10));
    const result = await adapter.createPool({
      name: "created",
      template: {},
      capacitySpec: { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0 },
    });

    expect(result.name).toBe("created");
    expect(result.capacitySpec.bufferMax).toBe(3);
    expect(result.status?.total).toBe(2);
    expect(result.createdAt).toBeInstanceOf(Date);
  });

  it("includes API key header", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const adapter = makeAdapter(201, makePoolResponse(), captured);
    await adapter.createPool({
      name: "p", template: {},
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    });
    expect(captured.requests[0].headers["open-sandbox-api-key"]).toBe("key-123");
  });

  it("throws SandboxApiException on 409 conflict", async () => {
    const adapter = makeAdapter(409, { code: "POOL_ALREADY_EXISTS", message: "already exists" });
    await expect(adapter.createPool({
      name: "dup", template: {},
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });

  it("throws SandboxApiException on 400 bad request", async () => {
    const adapter = makeAdapter(400, { code: "INVALID_REQUEST", message: "bad" });
    await expect(adapter.createPool({
      name: "bad", template: {},
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });

  it("throws SandboxApiException on 501 not supported", async () => {
    const adapter = makeAdapter(501, { code: "NOT_SUPPORTED", message: "non-k8s" });
    await expect(adapter.createPool({
      name: "p", template: {},
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });

  it("throws SandboxApiException on 500 server error", async () => {
    const adapter = makeAdapter(500, { code: "INTERNAL", message: "error" });
    await expect(adapter.createPool({
      name: "p", template: {},
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// getPool
// ---------------------------------------------------------------------------

describe("PoolsAdapter.getPool", () => {
  it("sends GET to /pools/{poolName}", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const adapter = makeAdapter(200, makePoolResponse("my-pool"), captured);

    await adapter.getPool("my-pool");

    const req = captured.requests[0];
    expect(req.method).toBe("GET");
    expect(req.url).toContain("/pools/my-pool");
  });

  it("returns mapped PoolInfo", async () => {
    const adapter = makeAdapter(200, makePoolResponse("my-pool", 2, 8));
    const result = await adapter.getPool("my-pool");
    expect(result.name).toBe("my-pool");
    expect(result.capacitySpec.poolMax).toBe(8);
  });

  it("throws SandboxApiException on 404", async () => {
    const adapter = makeAdapter(404, { code: "NOT_FOUND", message: "not found" });
    await expect(adapter.getPool("ghost")).rejects.toThrow(SandboxApiException);
  });

  it("throws SandboxApiException on 500", async () => {
    const adapter = makeAdapter(500, { code: "INTERNAL", message: "err" });
    await expect(adapter.getPool("p")).rejects.toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// listPools
// ---------------------------------------------------------------------------

describe("PoolsAdapter.listPools", () => {
  it("sends GET to /pools (no path segment)", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const listBody: PoolComponents["schemas"]["ListPoolsResponse"] = {
      items: [makePoolResponse("p1"), makePoolResponse("p2")],
    };
    const adapter = makeAdapter(200, listBody, captured);

    await adapter.listPools();

    const req = captured.requests[0];
    expect(req.method).toBe("GET");
    // URL should end in /pools (not /pools/something)
    expect(req.url).toMatch(/\/pools\/?$/);
  });

  it("returns all items", async () => {
    const listBody: PoolComponents["schemas"]["ListPoolsResponse"] = {
      items: [makePoolResponse("a"), makePoolResponse("b"), makePoolResponse("c")],
    };
    const adapter = makeAdapter(200, listBody);
    const result = await adapter.listPools();

    expect(result.items).toHaveLength(3);
    expect(result.items.map(p => p.name)).toEqual(["a", "b", "c"]);
  });

  it("returns empty array for empty list", async () => {
    const adapter = makeAdapter(200, { items: [] });
    const result = await adapter.listPools();
    expect(result.items).toHaveLength(0);
  });

  it("throws SandboxApiException on 500", async () => {
    const adapter = makeAdapter(500, { code: "INTERNAL", message: "err" });
    await expect(adapter.listPools()).rejects.toThrow(SandboxApiException);
  });

  it("throws SandboxApiException on 501", async () => {
    const adapter = makeAdapter(501, { code: "NOT_SUPPORTED", message: "non-k8s" });
    await expect(adapter.listPools()).rejects.toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// updatePool
// ---------------------------------------------------------------------------

describe("PoolsAdapter.updatePool", () => {
  it("sends PUT to /pools/{poolName}", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const adapter = makeAdapter(200, makePoolResponse("target", 9, 50), captured);

    await adapter.updatePool("target", {
      capacitySpec: { bufferMax: 9, bufferMin: 3, poolMax: 50, poolMin: 0 },
    });

    const req = captured.requests[0];
    expect(req.method).toBe("PUT");
    expect(req.url).toContain("/pools/target");
  });

  it("sends correct capacity in request body", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const adapter = makeAdapter(200, makePoolResponse(), captured);

    await adapter.updatePool("p", {
      capacitySpec: { bufferMax: 7, bufferMin: 3, poolMax: 30, poolMin: 2 },
    });

    const body = captured.requests[0].body as Record<string, unknown>;
    const cap = body.capacitySpec as Record<string, number>;
    expect(cap.bufferMax).toBe(7);
    expect(cap.poolMin).toBe(2);
  });

  it("returns updated PoolInfo", async () => {
    const adapter = makeAdapter(200, makePoolResponse("p", 9, 50));
    const result = await adapter.updatePool("p", {
      capacitySpec: { bufferMax: 9, bufferMin: 3, poolMax: 50, poolMin: 0 },
    });
    expect(result.capacitySpec.bufferMax).toBe(9);
    expect(result.capacitySpec.poolMax).toBe(50);
  });

  it("throws SandboxApiException on 404", async () => {
    const adapter = makeAdapter(404, { code: "NOT_FOUND", message: "not found" });
    await expect(adapter.updatePool("ghost", {
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });

  it("throws SandboxApiException on 500", async () => {
    const adapter = makeAdapter(500, { code: "INTERNAL", message: "err" });
    await expect(adapter.updatePool("p", {
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// deletePool
// ---------------------------------------------------------------------------

describe("PoolsAdapter.deletePool", () => {
  it("sends DELETE to /pools/{poolName}", async () => {
    const captured = { requests: [] as CapturedRequest[] };
    const adapter = new PoolsAdapter({
      baseUrl: "http://server.local/v1",
      apiKey: "key-123",
      fetch: (async (input: RequestInfo | URL, init?: RequestInit) => {
        let url: string;
        let method: string;
        if (input instanceof Request) {
          url = input.url;
          method = (input.method ?? "GET").toUpperCase();
        } else {
          url = typeof input === "string" ? input : String(input);
          method = ((init?.method ?? "GET") as string).toUpperCase();
        }
        captured.requests.push({ url, method, headers: {} });
        return new Response(null, { status: 204 });
      }) as unknown as typeof fetch,
    });

    await adapter.deletePool("bye-pool");

    expect(captured.requests).toHaveLength(1);
    const req = captured.requests[0];
    expect(req.method).toBe("DELETE");
    expect(req.url).toContain("/pools/bye-pool");
  });

  it("resolves (returns undefined) on success", async () => {
    const adapter = new PoolsAdapter({
      baseUrl: "http://server.local/v1",
      apiKey: "key-123",
      fetch: (async () => new Response(null, { status: 204 })) as unknown as typeof fetch,
    });
    await expect(adapter.deletePool("p")).resolves.toBeUndefined();
  });

  it("throws SandboxApiException on 404", async () => {
    const adapter = makeAdapter(404, { code: "NOT_FOUND", message: "not found" });
    await expect(adapter.deletePool("ghost")).rejects.toThrow(SandboxApiException);
  });

  it("throws SandboxApiException on 500", async () => {
    const adapter = makeAdapter(500, { code: "INTERNAL", message: "err" });
    await expect(adapter.deletePool("p")).rejects.toThrow(SandboxApiException);
  });
});
