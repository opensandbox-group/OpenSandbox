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

import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { EndpointCache } from "../src/core/endpointCache.js";

function ep(addr: string) {
  return { endpoint: addr, headers: {} };
}

describe("EndpointCache", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("returns undefined on miss", () => {
    const c = new EndpointCache();
    expect(c.get("sb-1", 8080, false)).toBeUndefined();
  });

  it("returns cached endpoint on hit", () => {
    const c = new EndpointCache();
    c.put("sb-1", 8080, false, ep("localhost:8080"));
    expect(c.get("sb-1", 8080, false)?.endpoint).toBe("localhost:8080");
  });

  it("expires entries after TTL", () => {
    const c = new EndpointCache({ ttlMs: 1000 });
    c.put("sb-1", 8080, false, ep("localhost:8080"));
    expect(c.get("sb-1", 8080, false)).toBeDefined();
    vi.advanceTimersByTime(1001);
    expect(c.get("sb-1", 8080, false)).toBeUndefined();
  });

  it("evicts LRU when at capacity", () => {
    const c = new EndpointCache({ maxSize: 3, ttlMs: 60000 });
    c.put("sb-0", 8080, false, ep("h0"));
    c.put("sb-1", 8080, false, ep("h1"));
    c.put("sb-2", 8080, false, ep("h2"));
    // Access sb-0 to make it recently used
    c.get("sb-0", 8080, false);
    // Insert 4th — sb-1 should be evicted (LRU)
    c.put("sb-3", 8080, false, ep("h3"));
    expect(c.get("sb-1", 8080, false)).toBeUndefined();
    expect(c.get("sb-0", 8080, false)?.endpoint).toBe("h0");
  });

  it("invalidates all entries for a sandbox", () => {
    const c = new EndpointCache();
    c.put("sb-1", 8080, false, ep("a"));
    c.put("sb-1", 18080, false, ep("b"));
    c.put("sb-2", 8080, false, ep("c"));
    c.invalidate("sb-1");
    expect(c.get("sb-1", 8080, false)).toBeUndefined();
    expect(c.get("sb-1", 18080, false)).toBeUndefined();
    expect(c.get("sb-2", 8080, false)?.endpoint).toBe("c");
  });

  it("deduplicates concurrent fetches", async () => {
    vi.useRealTimers();
    const c = new EndpointCache();
    let fetchCount = 0;
    const fetcher = async () => {
      fetchCount++;
      await new Promise((r) => setTimeout(r, 50));
      return ep("result");
    };

    const results = await Promise.all([
      c.getOrFetch("sb-1", 8080, false, fetcher),
      c.getOrFetch("sb-1", 8080, false, fetcher),
      c.getOrFetch("sb-1", 8080, false, fetcher),
    ]);

    expect(fetchCount).toBe(1);
    expect(results.every((r) => r.endpoint === "result")).toBe(true);
  });

  it("does not cache on fetch error", async () => {
    vi.useRealTimers();
    const c = new EndpointCache();
    const fetcher = async () => {
      throw new Error("network error");
    };

    await expect(c.getOrFetch("sb-1", 8080, false, fetcher)).rejects.toThrow("network error");
    expect(c.size).toBe(0);
  });

  it("returns cached value without calling fetcher", async () => {
    vi.useRealTimers();
    const c = new EndpointCache();
    c.put("sb-1", 8080, false, ep("cached"));
    let called = false;
    const result = await c.getOrFetch("sb-1", 8080, false, async () => {
      called = true;
      return ep("fetched");
    });
    expect(result.endpoint).toBe("cached");
    expect(called).toBe(false);
  });
});
