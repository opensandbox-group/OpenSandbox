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
 * Tests for PoolManagerSync – synchronous wrapper over PoolManager.
 *
 * Because the actual worker-thread mechanism requires a running Node.js
 * runtime with SharedArrayBuffer support, these tests verify:
 *
 * 1. Class API shape and method signatures.
 * 2. Error propagation via the worker result channel.
 * 3. Post-close guard.
 * 4. Payload construction (correct op + args forwarded to runPoolOpSync).
 *
 * We mock `runPoolOpSync` so no real worker threads are spawned.
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import { SandboxApiException } from "../src/core/exceptions.js";

// ---------------------------------------------------------------------------
// Module-level mock of runPoolOpSync
// ---------------------------------------------------------------------------

// We mock the module before importing PoolManagerSync so the mock is in place
// when the module initialises.
vi.mock("../src/sync/runSync.js", () => ({
  runPoolOpSync: vi.fn(),
}));

// Import after mock setup
import { PoolManagerSync } from "../src/poolManagerSync.js";
import { runPoolOpSync } from "../src/sync/runSync.js";

const mockRunSync = vi.mocked(runPoolOpSync);

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function makePoolInfo(name = "test-pool", bufferMax = 3, poolMax = 10) {
  return {
    name,
    capacitySpec: { bufferMax, bufferMin: 1, poolMax, poolMin: 0 },
    status: { total: 2, allocated: 1, available: 1, revision: "rev-1" },
    createdAt: "2025-06-01T00:00:00.000Z",
  };
}

function makeManager(): PoolManagerSync {
  return PoolManagerSync.create({
    connectionConfig: { apiKey: "test-key", domain: "localhost:8080" },
  });
}

beforeEach(() => {
  vi.clearAllMocks();
});

// ---------------------------------------------------------------------------
// API shape
// ---------------------------------------------------------------------------

describe("PoolManagerSync API shape", () => {
  it("create() returns a PoolManagerSync instance", () => {
    const manager = PoolManagerSync.create();
    expect(manager).toBeInstanceOf(PoolManagerSync);
  });

  it("create() with default options returns a PoolManagerSync instance", () => {
    const manager = PoolManagerSync.create();
    expect(manager).toBeInstanceOf(PoolManagerSync);
  });

  it("exposes createPool, getPool, listPools, updatePool, deletePool, close", () => {
    const manager = PoolManagerSync.create();
    expect(typeof manager.createPool).toBe("function");
    expect(typeof manager.getPool).toBe("function");
    expect(typeof manager.listPools).toBe("function");
    expect(typeof manager.updatePool).toBe("function");
    expect(typeof manager.deletePool).toBe("function");
    expect(typeof manager.close).toBe("function");
  });

  it("exposes Symbol.dispose", () => {
    const manager = PoolManagerSync.create();
    expect(typeof manager[Symbol.dispose]).toBe("function");
  });
});

// ---------------------------------------------------------------------------
// createPool
// ---------------------------------------------------------------------------

describe("PoolManagerSync.createPool", () => {
  it("calls runPoolOpSync with op=createPool and request body", () => {
    const info = makePoolInfo("new-pool");
    mockRunSync.mockReturnValueOnce(info);

    const manager = makeManager();
    const req = {
      name: "new-pool",
      template: { spec: {} },
      capacitySpec: { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0 },
    };
    const result = manager.createPool(req);

    expect(mockRunSync).toHaveBeenCalledOnce();
    const [, , payload] = mockRunSync.mock.calls[0];
    expect(payload.op).toBe("createPool");
    if (payload.op === "createPool") {
      expect(payload.req.name).toBe("new-pool");
      expect(payload.req.capacitySpec.bufferMax).toBe(3);
    }
    expect(result).toEqual(info);
  });

  it("returns PoolInfo from worker result", () => {
    const info = makePoolInfo("created", 5, 20);
    mockRunSync.mockReturnValueOnce(info);

    const manager = makeManager();
    const result = manager.createPool({
      name: "created",
      template: {},
      capacitySpec: { bufferMax: 5, bufferMin: 2, poolMax: 20, poolMin: 0 },
    });

    expect(result.name).toBe("created");
    expect(result.capacitySpec.bufferMax).toBe(5);
    expect(result.capacitySpec.poolMax).toBe(20);
  });

  it("propagates SandboxApiException thrown by runPoolOpSync", () => {
    mockRunSync.mockImplementationOnce(() => {
      throw new SandboxApiException({ message: "already exists", statusCode: 409 });
    });

    const manager = makeManager();
    expect(() =>
      manager.createPool({
        name: "dup",
        template: {},
        capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
      })
    ).toThrow(SandboxApiException);
  });

  it("propagates generic Error thrown by runPoolOpSync", () => {
    mockRunSync.mockImplementationOnce(() => {
      throw new Error("network failure");
    });

    const manager = makeManager();
    expect(() =>
      manager.createPool({
        name: "p",
        template: {},
        capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
      })
    ).toThrow("network failure");
  });
});

// ---------------------------------------------------------------------------
// getPool
// ---------------------------------------------------------------------------

describe("PoolManagerSync.getPool", () => {
  it("calls runPoolOpSync with op=getPool and pool name", () => {
    const info = makePoolInfo("my-pool");
    mockRunSync.mockReturnValueOnce(info);

    const manager = makeManager();
    manager.getPool("my-pool");

    const [, , payload] = mockRunSync.mock.calls[0];
    expect(payload.op).toBe("getPool");
    if (payload.op === "getPool") {
      expect(payload.poolName).toBe("my-pool");
    }
  });

  it("returns pool info", () => {
    const info = makePoolInfo("p1", 2, 8);
    mockRunSync.mockReturnValueOnce(info);

    const manager = makeManager();
    const result = manager.getPool("p1");
    expect(result.name).toBe("p1");
    expect(result.capacitySpec.poolMax).toBe(8);
  });

  it("throws SandboxApiException on 404", () => {
    mockRunSync.mockImplementationOnce(() => {
      throw new SandboxApiException({ message: "not found", statusCode: 404 });
    });

    const manager = makeManager();
    const err = (() => {
      try {
        manager.getPool("ghost");
      } catch (e) {
        return e;
      }
    })();
    expect(err).toBeInstanceOf(SandboxApiException);
    expect((err as SandboxApiException).statusCode).toBe(404);
  });
});

// ---------------------------------------------------------------------------
// listPools
// ---------------------------------------------------------------------------

describe("PoolManagerSync.listPools", () => {
  it("calls runPoolOpSync with op=listPools", () => {
    mockRunSync.mockReturnValueOnce({ items: [] });

    const manager = makeManager();
    manager.listPools();

    const [, , payload] = mockRunSync.mock.calls[0];
    expect(payload.op).toBe("listPools");
  });

  it("returns pool list", () => {
    const list = {
      items: [makePoolInfo("a"), makePoolInfo("b"), makePoolInfo("c")],
    };
    mockRunSync.mockReturnValueOnce(list);

    const manager = makeManager();
    const result = manager.listPools();
    expect(result.items).toHaveLength(3);
    expect(result.items.map((p) => p.name)).toEqual(["a", "b", "c"]);
  });

  it("returns empty list", () => {
    mockRunSync.mockReturnValueOnce({ items: [] });

    const manager = makeManager();
    const result = manager.listPools();
    expect(result.items).toHaveLength(0);
  });

  it("propagates SandboxApiException on 501", () => {
    mockRunSync.mockImplementationOnce(() => {
      throw new SandboxApiException({ message: "not supported", statusCode: 501 });
    });

    const manager = makeManager();
    expect(() => manager.listPools()).toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// updatePool
// ---------------------------------------------------------------------------

describe("PoolManagerSync.updatePool", () => {
  it("calls runPoolOpSync with op=updatePool, poolName and request", () => {
    const info = makePoolInfo("target", 9, 50);
    mockRunSync.mockReturnValueOnce(info);

    const manager = makeManager();
    manager.updatePool("target", {
      capacitySpec: { bufferMax: 9, bufferMin: 3, poolMax: 50, poolMin: 0 },
    });

    const [, , payload] = mockRunSync.mock.calls[0];
    expect(payload.op).toBe("updatePool");
    if (payload.op === "updatePool") {
      expect(payload.poolName).toBe("target");
      expect(payload.req.capacitySpec.bufferMax).toBe(9);
      expect(payload.req.capacitySpec.poolMax).toBe(50);
    }
  });

  it("returns updated pool info", () => {
    const info = makePoolInfo("p", 9, 50);
    mockRunSync.mockReturnValueOnce(info);

    const manager = makeManager();
    const result = manager.updatePool("p", {
      capacitySpec: { bufferMax: 9, bufferMin: 3, poolMax: 50, poolMin: 0 },
    });
    expect(result.capacitySpec.bufferMax).toBe(9);
    expect(result.capacitySpec.poolMax).toBe(50);
  });

  it("throws SandboxApiException on 404", () => {
    mockRunSync.mockImplementationOnce(() => {
      throw new SandboxApiException({ message: "not found", statusCode: 404 });
    });

    const manager = makeManager();
    expect(() =>
      manager.updatePool("ghost", {
        capacitySpec: { bufferMax: 1, bufferMin: 0, poolMax: 5, poolMin: 0 },
      })
    ).toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// deletePool
// ---------------------------------------------------------------------------

describe("PoolManagerSync.deletePool", () => {
  it("calls runPoolOpSync with op=deletePool and pool name", () => {
    mockRunSync.mockReturnValueOnce(undefined);

    const manager = makeManager();
    manager.deletePool("bye-pool");

    const [, , payload] = mockRunSync.mock.calls[0];
    expect(payload.op).toBe("deletePool");
    if (payload.op === "deletePool") {
      expect(payload.poolName).toBe("bye-pool");
    }
  });

  it("returns undefined on success", () => {
    mockRunSync.mockReturnValueOnce(undefined);

    const manager = makeManager();
    const result = manager.deletePool("p");
    expect(result).toBeUndefined();
  });

  it("throws SandboxApiException on 404", () => {
    mockRunSync.mockImplementationOnce(() => {
      throw new SandboxApiException({ message: "not found", statusCode: 404 });
    });

    const manager = makeManager();
    expect(() => manager.deletePool("ghost")).toThrow(SandboxApiException);
  });
});

// ---------------------------------------------------------------------------
// close / post-close guard
// ---------------------------------------------------------------------------

describe("PoolManagerSync.close", () => {
  it("close() marks manager as closed", () => {
    mockRunSync.mockReturnValue({ items: [] });

    const manager = makeManager();
    manager.close();

    // Any operation after close should throw.
    expect(() => manager.listPools()).toThrow("PoolManagerSync has been closed");
  });

  it("Symbol.dispose closes the manager", () => {
    mockRunSync.mockReturnValue({ items: [] });

    const manager = makeManager();
    manager[Symbol.dispose]();

    expect(() => manager.listPools()).toThrow("PoolManagerSync has been closed");
  });

  it("close() is idempotent", () => {
    const manager = makeManager();
    manager.close();
    expect(() => manager.close()).not.toThrow();
  });
});

// ---------------------------------------------------------------------------
// Connection config forwarding
// ---------------------------------------------------------------------------

describe("PoolManagerSync connection config forwarding", () => {
  it("passes connection options to runPoolOpSync", () => {
    mockRunSync.mockReturnValueOnce({ items: [] });

    const manager = PoolManagerSync.create({
      connectionConfig: { apiKey: "my-key", domain: "api.example.com" },
    });
    manager.listPools();

    const [, managerOptions] = mockRunSync.mock.calls[0];
    const opts = managerOptions as { connectionConfig?: { apiKey?: string; domain?: string } };
    expect(opts.connectionConfig?.apiKey).toBe("my-key");
    expect(opts.connectionConfig?.domain).toBe("api.example.com");
  });

  it("forwards default options (empty object) when none provided", () => {
    mockRunSync.mockReturnValueOnce({ items: [] });

    const manager = PoolManagerSync.create();
    manager.listPools();

    const [, managerOptions] = mockRunSync.mock.calls[0];
    // Should not throw; options forwarded as-is.
    expect(managerOptions).toBeDefined();
  });
});
