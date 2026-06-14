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
 * Tests for Pool domain model types and PoolsAdapter mapping helpers.
 *
 * These tests exercise the adapter's internal mapping functions
 * without making any HTTP calls.
 */

import { describe, it, expect } from "vitest";
import { PoolsAdapter } from "../src/adapters/poolsAdapter.js";
import type { PoolComponents } from "../src/api/lifecycle.js";

// ---- helpers to build raw API payloads -----

function makeRawCapacity(
  overrides: Partial<PoolComponents["schemas"]["PoolCapacitySpec"]> = {}
): PoolComponents["schemas"]["PoolCapacitySpec"] {
  return { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0, ...overrides };
}

function makeRawStatus(
  overrides: Partial<PoolComponents["schemas"]["PoolStatus"]> = {}
): PoolComponents["schemas"]["PoolStatus"] {
  return { total: 2, allocated: 1, available: 1, revision: "rev-1", ...overrides };
}

function makeRawPool(
  name = "test-pool",
  withStatus = true,
  withCreatedAt = true
): PoolComponents["schemas"]["PoolResponse"] {
  return {
    name,
    capacitySpec: makeRawCapacity(),
    status: withStatus ? makeRawStatus() : undefined,
    createdAt: withCreatedAt ? "2025-01-01T00:00:00.000Z" : undefined,
  };
}

// ---------------------------------------------------------------------------
// model types
// ---------------------------------------------------------------------------

describe("Pool model shape", () => {
  it("PoolCapacitySpec has expected fields", () => {
    // Compile-time verification: if a field is wrong this file won't compile.
    const spec: import("../src/models/pools.js").PoolCapacitySpec = {
      bufferMax: 3,
      bufferMin: 1,
      poolMax: 10,
      poolMin: 0,
    };
    expect(spec.bufferMax).toBe(3);
    expect(spec.poolMax).toBe(10);
  });

  it("PoolInfo allows optional status and createdAt", () => {
    const info: import("../src/models/pools.js").PoolInfo = {
      name: "p",
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    };
    expect(info.status).toBeUndefined();
    expect(info.createdAt).toBeUndefined();
  });

  it("PoolListResponse items array", () => {
    const resp: import("../src/models/pools.js").PoolListResponse = { items: [] };
    expect(resp.items).toHaveLength(0);
  });

  it("CreatePoolRequest has name, template, capacitySpec", () => {
    const req: import("../src/models/pools.js").CreatePoolRequest = {
      name: "my-pool",
      template: { spec: {} },
      capacitySpec: { bufferMax: 2, bufferMin: 1, poolMax: 8, poolMin: 0 },
    };
    expect(req.name).toBe("my-pool");
  });

  it("UpdatePoolRequest has capacitySpec", () => {
    const req: import("../src/models/pools.js").UpdatePoolRequest = {
      capacitySpec: { bufferMax: 5, bufferMin: 2, poolMax: 20, poolMin: 0 },
    };
    expect(req.capacitySpec.poolMax).toBe(20);
  });
});

// ---------------------------------------------------------------------------
// PoolsAdapter mapping helpers (via createPool / getPool / listPools which
// go through the same internal mapPoolInfo helper)
// ---------------------------------------------------------------------------

describe("PoolsAdapter – internal mapping", () => {
  // We access the private static-like helpers by making a real adapter with
  // a mocked fetch, then checking the returned domain model shapes.

  function makeMockFetch(status: number, body: unknown): typeof fetch {
    return async (_input, _init) =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "Content-Type": "application/json" },
      });
  }

  function makeAdapter(status: number, body: unknown): PoolsAdapter {
    return new PoolsAdapter({
      baseUrl: "http://test.local/v1",
      apiKey: "test-key",
      fetch: makeMockFetch(status, body) as unknown as typeof fetch,
    });
  }

  describe("capacitySpec mapping", () => {
    it("maps all four capacity fields correctly", async () => {
      const raw = makeRawPool();
      const adapter = makeAdapter(201, raw);
      const result = await adapter.createPool({
        name: "test-pool",
        template: {},
        capacitySpec: { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0 },
      });
      expect(result.capacitySpec.bufferMax).toBe(3);
      expect(result.capacitySpec.bufferMin).toBe(1);
      expect(result.capacitySpec.poolMax).toBe(10);
      expect(result.capacitySpec.poolMin).toBe(0);
    });
  });

  describe("status mapping", () => {
    it("maps status fields when present", async () => {
      const raw = makeRawPool("p", true);
      const adapter = makeAdapter(200, raw);
      const result = await adapter.getPool("p");
      expect(result.status).toBeDefined();
      expect(result.status?.total).toBe(2);
      expect(result.status?.allocated).toBe(1);
      expect(result.status?.available).toBe(1);
      expect(result.status?.revision).toBe("rev-1");
    });

    it("status is undefined when absent in response", async () => {
      const raw = makeRawPool("p", false);
      const adapter = makeAdapter(200, raw);
      const result = await adapter.getPool("p");
      expect(result.status).toBeUndefined();
    });
  });

  describe("createdAt mapping", () => {
    it("parses ISO createdAt to Date", async () => {
      const raw = makeRawPool("p", true, true);
      const adapter = makeAdapter(200, raw);
      const result = await adapter.getPool("p");
      expect(result.createdAt).toBeInstanceOf(Date);
      expect(result.createdAt?.getFullYear()).toBe(2025);
    });

    it("createdAt is undefined when absent in response", async () => {
      const raw = makeRawPool("p", true, false);
      const adapter = makeAdapter(200, raw);
      const result = await adapter.getPool("p");
      expect(result.createdAt).toBeUndefined();
    });
  });

  describe("name mapping", () => {
    it("preserves pool name", async () => {
      const adapter = makeAdapter(200, makeRawPool("my-special-pool"));
      const result = await adapter.getPool("my-special-pool");
      expect(result.name).toBe("my-special-pool");
    });
  });

  describe("listPools item mapping", () => {
    it("maps each item in list response", async () => {
      const listBody: PoolComponents["schemas"]["ListPoolsResponse"] = {
        items: [makeRawPool("pool-a"), makeRawPool("pool-b", false, false)],
      };
      const adapter = makeAdapter(200, listBody);
      const result = await adapter.listPools();
      expect(result.items).toHaveLength(2);
      expect(result.items[0].name).toBe("pool-a");
      expect(result.items[1].name).toBe("pool-b");
      expect(result.items[0].status).toBeDefined();
      expect(result.items[1].status).toBeUndefined();
    });

    it("returns empty items array for empty list", async () => {
      const adapter = makeAdapter(200, { items: [] });
      const result = await adapter.listPools();
      expect(result.items).toHaveLength(0);
    });
  });
});
