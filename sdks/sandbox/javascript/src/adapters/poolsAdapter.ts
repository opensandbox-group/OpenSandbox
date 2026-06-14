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

import createClient from "openapi-fetch";
import type { Client } from "openapi-fetch";

import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { poolPaths, PoolComponents } from "../api/lifecycle.js";
import type { Pools } from "../services/pools.js";
import type {
  CreatePoolRequest,
  PoolCapacitySpec,
  PoolInfo,
  PoolListResponse,
  PoolStatus,
  UpdatePoolRequest,
} from "../models/pools.js";

type PoolClient = Client<poolPaths>;

type ApiPoolResponse = PoolComponents["schemas"]["PoolResponse"];
type ApiPoolCapacitySpec = PoolComponents["schemas"]["PoolCapacitySpec"];
type ApiPoolStatus = PoolComponents["schemas"]["PoolStatus"];

// ---- helpers ---------------------------------------------------------------

function parseOptionalDate(field: string, v: unknown): Date | undefined {
  if (v === undefined || v === null) return undefined;
  if (typeof v !== "string" || !v) {
    throw new Error(`Invalid ${field}: expected ISO string, got ${typeof v}`);
  }
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) throw new Error(`Invalid ${field}: ${v}`);
  return d;
}

function mapCapacitySpec(raw: ApiPoolCapacitySpec): PoolCapacitySpec {
  return {
    bufferMax: raw.bufferMax,
    bufferMin: raw.bufferMin,
    poolMax: raw.poolMax,
    poolMin: raw.poolMin,
  };
}

function mapPoolStatus(raw: ApiPoolStatus): PoolStatus {
  return {
    total: raw.total,
    allocated: raw.allocated,
    available: raw.available,
    revision: raw.revision,
  };
}

function mapPoolInfo(raw: ApiPoolResponse): PoolInfo {
  return {
    name: raw.name,
    capacitySpec: mapCapacitySpec(raw.capacitySpec),
    status: raw.status ? mapPoolStatus(raw.status) : undefined,
    createdAt: parseOptionalDate("createdAt", raw.createdAt),
  };
}

// ---- adapter ---------------------------------------------------------------

/**
 * HTTP adapter implementing the {@link Pools} service interface.
 *
 * Uses an `openapi-fetch` client typed against the pool path definitions in
 * `api/lifecycle.ts` to ensure the request/response shapes stay in sync.
 */
export class PoolsAdapter implements Pools {
  private readonly client: PoolClient;

  constructor(opts: {
    baseUrl?: string;
    apiKey?: string;
    headers?: Record<string, string>;
    fetch?: typeof fetch;
  }) {
    const headers: Record<string, string> = { ...(opts.headers ?? {}) };
    if (opts.apiKey && !headers["OPEN-SANDBOX-API-KEY"]) {
      headers["OPEN-SANDBOX-API-KEY"] = opts.apiKey;
    }

    const createClientFn =
      (createClient as unknown as { default?: typeof createClient }).default ??
      createClient;
    this.client = createClientFn<poolPaths>({
      baseUrl: opts.baseUrl ?? "http://localhost:8080/v1",
      headers,
      fetch: opts.fetch,
    });
  }

  async createPool(req: CreatePoolRequest): Promise<PoolInfo> {
    const { data, error, response } = await this.client.POST("/pools", {
      body: req as PoolComponents["schemas"]["CreatePoolRequest"],
    });
    throwOnOpenApiFetchError({ error, response }, "Create pool failed");
    const raw = data as ApiPoolResponse | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("Create pool failed: unexpected response shape");
    }
    return mapPoolInfo(raw);
  }

  async getPool(poolName: string): Promise<PoolInfo> {
    const { data, error, response } = await this.client.GET("/pools/{poolName}", {
      params: { path: { poolName } },
    });
    throwOnOpenApiFetchError({ error, response }, `Get pool '${poolName}' failed`);
    const raw = data as ApiPoolResponse | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error(`Get pool '${poolName}' failed: unexpected response shape`);
    }
    return mapPoolInfo(raw);
  }

  async listPools(): Promise<PoolListResponse> {
    const { data, error, response } = await this.client.GET("/pools", {});
    throwOnOpenApiFetchError({ error, response }, "List pools failed");
    const raw = data as PoolComponents["schemas"]["ListPoolsResponse"] | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("List pools failed: unexpected response shape");
    }
    const items = Array.isArray(raw.items) ? raw.items.map(mapPoolInfo) : [];
    return { items };
  }

  async updatePool(poolName: string, req: UpdatePoolRequest): Promise<PoolInfo> {
    const { data, error, response } = await this.client.PUT("/pools/{poolName}", {
      params: { path: { poolName } },
      body: req as PoolComponents["schemas"]["UpdatePoolRequest"],
    });
    throwOnOpenApiFetchError({ error, response }, `Update pool '${poolName}' failed`);
    const raw = data as ApiPoolResponse | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error(`Update pool '${poolName}' failed: unexpected response shape`);
    }
    return mapPoolInfo(raw);
  }

  async deletePool(poolName: string): Promise<void> {
    const { error, response } = await this.client.DELETE("/pools/{poolName}", {
      params: { path: { poolName } },
    });
    throwOnOpenApiFetchError({ error, response }, `Delete pool '${poolName}' failed`);
  }
}
