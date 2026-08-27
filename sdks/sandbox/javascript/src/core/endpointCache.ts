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

import type { Endpoint } from "../models/sandboxes.js";

interface CacheEntry {
  endpoint: Endpoint;
  expiresAt: number;
}

/**
 * LRU + TTL endpoint cache with inflight deduplication.
 *
 * JS is single-threaded so no mutex needed — but concurrent async calls
 * can overlap, so inflight dedup uses a shared Promise per key.
 */
export class EndpointCache {
  private readonly cache = new Map<string, CacheEntry>();
  private readonly inflight = new Map<string, Promise<Endpoint>>();
  private readonly maxSize: number;
  private readonly ttlMs: number;
  private generation = 0;

  constructor(opts: { maxSize?: number; ttlMs?: number } = {}) {
    this.maxSize = opts.maxSize ?? 1024;
    this.ttlMs = opts.ttlMs ?? 600_000;
  }

  private cloneEndpoint(ep: Endpoint): Endpoint {
    return { ...ep, headers: ep.headers ? { ...ep.headers } : {} };
  }

  private makeKey(sandboxId: string, port: number, useServerProxy: boolean): string {
    return `${sandboxId}:${port}:${useServerProxy}`;
  }

  get(sandboxId: string, port: number, useServerProxy: boolean): Endpoint | undefined {
    const key = this.makeKey(sandboxId, port, useServerProxy);
    const entry = this.cache.get(key);
    if (!entry) return undefined;
    if (Date.now() >= entry.expiresAt) {
      this.cache.delete(key);
      return undefined;
    }
    // Move to end (most recently used) — delete + re-set.
    this.cache.delete(key);
    this.cache.set(key, entry);
    return this.cloneEndpoint(entry.endpoint);
  }

  put(sandboxId: string, port: number, useServerProxy: boolean, endpoint: Endpoint): void {
    const key = this.makeKey(sandboxId, port, useServerProxy);
    this.cache.delete(key);
    while (this.cache.size >= this.maxSize) {
      const oldest = this.cache.keys().next().value;
      if (oldest !== undefined) this.cache.delete(oldest);
      else break;
    }
    this.cache.set(key, { endpoint: this.cloneEndpoint(endpoint), expiresAt: Date.now() + this.ttlMs });
  }

  invalidate(sandboxId: string): void {
    this.generation++;
    const prefix = `${sandboxId}:`;
    for (const key of [...this.cache.keys()]) {
      if (key.startsWith(prefix)) {
        this.cache.delete(key);
      }
    }
    for (const key of [...this.inflight.keys()]) {
      if (key.startsWith(prefix)) {
        this.inflight.delete(key);
      }
    }
  }

  async getOrFetch(
    sandboxId: string,
    port: number,
    useServerProxy: boolean,
    fetcher: () => Promise<Endpoint>
  ): Promise<Endpoint> {
    const cached = this.get(sandboxId, port, useServerProxy);
    if (cached) return cached;

    const key = this.makeKey(sandboxId, port, useServerProxy);

    const existing = this.inflight.get(key);
    if (existing) return existing.then((ep) => this.cloneEndpoint(ep));

    const genBefore = this.generation;
    const promise = fetcher()
      .then((ep) => {
        // Only cache if no invalidation occurred during fetch.
        if (this.generation === genBefore) {
          this.put(sandboxId, port, useServerProxy, ep);
        }
        this.inflight.delete(key);
        return this.cloneEndpoint(ep);
      })
      .catch((err) => {
        this.inflight.delete(key);
        throw err;
      });

    this.inflight.set(key, promise);
    return promise;
  }

  get size(): number {
    return this.cache.size;
  }
}
