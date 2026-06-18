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
 * Tests for PoolManager – business logic, delegation to service, lifecycle.
 */

import { describe, it, expect, vi } from "vitest";
import { PoolManager } from "../src/poolManager.js";
import { SandboxApiException } from "../src/core/exceptions.js";
import type { Pools } from "../src/services/pools.js";
import type { PoolInfo, PoolListResponse } from "../src/models/pools.js";

// ---------------------------------------------------------------------------
// Stub Pools service
// ---------------------------------------------------------------------------

function makePoolInfo(name: string, bufferMax = 3, poolMax = 10): PoolInfo {
  return {
    name,
    capacitySpec: { bufferMax, bufferMin: 1, poolMax, poolMin: 0 },
    status: { total: 0, allocated: 0, available: 0, revision: "init" },
  };
}

class PoolsStub implements Pools {
  private store = new Map<string, PoolInfo>();
  readonly createCalls: import("../src/models/pools.js").CreatePoolRequest[] = [];
  readonly updateCalls: Array<{ poolName: string; req: import("../src/models/pools.js").UpdatePoolRequest }> = [];
  readonly deleteCalls: string[] = [];

  async createPool(req: import("../src/models/pools.js").CreatePoolRequest): Promise<PoolInfo> {
    this.createCalls.push(req);
    const info = makePoolInfo(req.name, req.capacitySpec.bufferMax, req.capacitySpec.poolMax);
    this.store.set(req.name, info);
    return info;
  }

  async getPool(poolName: string): Promise<PoolInfo> {
    const info = this.store.get(poolName);
    if (!info) throw new SandboxApiException({ message: `Pool '${poolName}' not found.`, statusCode: 404 });
    return info;
  }

  async listPools(): Promise<PoolListResponse> {
    return { items: Array.from(this.store.values()) };
  }

  async updatePool(poolName: string, req: import("../src/models/pools.js").UpdatePoolRequest): Promise<PoolInfo> {
    this.updateCalls.push({ poolName, req });
    if (!this.store.has(poolName)) {
      throw new SandboxApiException({ message: `Pool '${poolName}' not found.`, statusCode: 404 });
    }
    const updated = makePoolInfo(poolName, req.capacitySpec.bufferMax, req.capacitySpec.poolMax);
    this.store.set(poolName, updated);
    return updated;
  }

  async deletePool(poolName: string): Promise<void> {
    this.deleteCalls.push(poolName);
    if (!this.store.has(poolName)) {
      throw new SandboxApiException({ message: `Pool '${poolName}' not found.`, statusCode: 404 });
    }
    this.store.delete(poolName);
  }
}

function makeManager(): { manager: PoolManager; stub: PoolsStub } {
  const stub = new PoolsStub();
  // Inject stub via the private constructor accessor
  const manager = Object.create(PoolManager.prototype) as PoolManager;
  (manager as any).pools = stub;
  (manager as any).connectionConfig = { closeTransport: async () => {} };
  return { manager, stub };
}

// ---------------------------------------------------------------------------
// createPool
// ---------------------------------------------------------------------------

describe("PoolManager.createPool", () => {
  it("returns PoolInfo with correct fields", async () => {
    const { manager } = makeManager();
    const pool = await manager.createPool({
      name: "ci-pool",
      template: { spec: {} },
      capacitySpec: { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0 },
    });
    expect(pool.name).toBe("ci-pool");
    expect(pool.capacitySpec.bufferMax).toBe(3);
    expect(pool.capacitySpec.poolMax).toBe(10);
  });

  it("delegates to Pools service with the full request", async () => {
    const { manager, stub } = makeManager();
    await manager.createPool({
      name: "my-pool",
      template: { spec: { containers: [] } },
      capacitySpec: { bufferMax: 5, bufferMin: 2, poolMax: 20, poolMin: 1 },
    });
    expect(stub.createCalls).toHaveLength(1);
    const req = stub.createCalls[0];
    expect(req.name).toBe("my-pool");
    expect(req.capacitySpec.bufferMax).toBe(5);
    expect(req.capacitySpec.poolMin).toBe(1);
  });

  it("propagates SandboxApiException on 409", async () => {
    const { manager, stub } = makeManager();
    stub.createPool = vi.fn().mockRejectedValue(
      new SandboxApiException({ message: "already exists", statusCode: 409 })
    );
    await expect(manager.createPool({
      name: "dup", template: {},
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// getPool
// ---------------------------------------------------------------------------

describe("PoolManager.getPool", () => {
  it("returns existing pool info", async () => {
    const { manager } = makeManager();
    await manager.createPool({
      name: "p1", template: {},
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    });
    const pool = await manager.getPool("p1");
    expect(pool.name).toBe("p1");
  });

  it("throws SandboxApiException with 404 for missing pool", async () => {
    const { manager } = makeManager();
    const err = await manager.getPool("ghost").catch(e => e);
    expect(err).toBeInstanceOf(SandboxApiException);
    expect((err as SandboxApiException).statusCode).toBe(404);
  });

  it("delegates with correct pool name", async () => {
    const { manager, stub } = makeManager();
    stub.getPool = vi.fn().mockResolvedValue(makePoolInfo("target"));
    await manager.getPool("target");
    expect(stub.getPool).toHaveBeenCalledWith("target");
  });
});

// ---------------------------------------------------------------------------
// listPools
// ---------------------------------------------------------------------------

describe("PoolManager.listPools", () => {
  it("returns empty list when no pools", async () => {
    const { manager } = makeManager();
    const result = await manager.listPools();
    expect(result.items).toHaveLength(0);
  });

  it("returns all pools", async () => {
    const { manager } = makeManager();
    await manager.createPool({ name: "a", template: {}, capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 } });
    await manager.createPool({ name: "b", template: {}, capacitySpec: { bufferMax: 2, bufferMin: 1, poolMax: 8, poolMin: 0 } });
    const result = await manager.listPools();
    expect(result.items).toHaveLength(2);
    expect(result.items.map(p => p.name)).toContain("a");
    expect(result.items.map(p => p.name)).toContain("b");
  });

  it("delegates to Pools service", async () => {
    const { manager, stub } = makeManager();
    stub.listPools = vi.fn().mockResolvedValue({ items: [] });
    await manager.listPools();
    expect(stub.listPools).toHaveBeenCalledOnce();
  });
});

// ---------------------------------------------------------------------------
// updatePool
// ---------------------------------------------------------------------------

describe("PoolManager.updatePool", () => {
  it("updates capacity and returns new PoolInfo", async () => {
    const { manager } = makeManager();
    await manager.createPool({ name: "p", template: {}, capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 } });

    const updated = await manager.updatePool("p", {
      capacitySpec: { bufferMax: 9, bufferMin: 3, poolMax: 50, poolMin: 0 },
    });
    expect(updated.capacitySpec.bufferMax).toBe(9);
    expect(updated.capacitySpec.poolMax).toBe(50);
  });

  it("delegates with correct poolName and request", async () => {
    const { manager, stub } = makeManager();
    await manager.createPool({ name: "p", template: {}, capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 } });

    await manager.updatePool("p", {
      capacitySpec: { bufferMax: 7, bufferMin: 2, poolMax: 30, poolMin: 0 },
    });
    expect(stub.updateCalls).toHaveLength(1);
    expect(stub.updateCalls[0].poolName).toBe("p");
    expect(stub.updateCalls[0].req.capacitySpec.bufferMax).toBe(7);
  });

  it("throws SandboxApiException on 404 for missing pool", async () => {
    const { manager } = makeManager();
    await expect(manager.updatePool("ghost", {
      capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
    })).rejects.toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// deletePool
// ---------------------------------------------------------------------------

describe("PoolManager.deletePool", () => {
  it("successfully deletes an existing pool", async () => {
    const { manager, stub } = makeManager();
    await manager.createPool({ name: "bye", template: {}, capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 } });
    await manager.deletePool("bye");
    expect(stub.deleteCalls).toContain("bye");
    expect(stub["store"].has("bye")).toBe(false);
  });

  it("delegates with correct pool name", async () => {
    const { manager, stub } = makeManager();
    stub.deletePool = vi.fn().mockResolvedValue(undefined);
    await manager.deletePool("to-delete");
    expect(stub.deletePool).toHaveBeenCalledWith("to-delete");
  });

  it("throws SandboxApiException on 404 for missing pool", async () => {
    const { manager } = makeManager();
    await expect(manager.deletePool("ghost")).rejects.toThrow(SandboxApiException);
  });

  it("resolves to undefined on success", async () => {
    const { manager } = makeManager();
    await manager.createPool({ name: "x", template: {}, capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 } });
    await expect(manager.deletePool("x")).resolves.toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// PoolManager.create factory
// ---------------------------------------------------------------------------

describe("PoolManager.create", () => {
  it("creates a PoolManager instance", () => {
    const manager = PoolManager.create({
      connectionConfig: { apiKey: "test-key", baseUrl: "http://localhost:8080" },
    });
    expect(manager).toBeInstanceOf(PoolManager);
  });

  it("creates with default options", () => {
    const manager = PoolManager.create();
    expect(manager).toBeInstanceOf(PoolManager);
  });

  it("exposes createPool, getPool, listPools, updatePool, deletePool methods", () => {
    const manager = PoolManager.create();
    expect(typeof manager.createPool).toBe("function");
    expect(typeof manager.getPool).toBe("function");
    expect(typeof manager.listPools).toBe("function");
    expect(typeof manager.updatePool).toBe("function");
    expect(typeof manager.deletePool).toBe("function");
    expect(typeof manager.close).toBe("function");
  });
});

// ---------------------------------------------------------------------------
// close
// ---------------------------------------------------------------------------

describe("PoolManager.close", () => {
  it("calls closeTransport on the connectionConfig", async () => {
    const { manager } = makeManager();
    const closeSpy = vi.fn().mockResolvedValue(undefined);
    (manager as any).connectionConfig = { closeTransport: closeSpy };
    await manager.close();
    expect(closeSpy).toHaveBeenCalledOnce();
  });
});
