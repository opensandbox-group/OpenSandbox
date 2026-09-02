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
 * Synchronous PoolManager for Node.js.
 *
 * Wraps the async {@link PoolManager} so that every method **blocks** the
 * calling thread until the operation completes.  Internally each call spawns
 * a short-lived worker thread that runs the async operation; the main thread
 * blocks on `Atomics.wait` until the worker posts a result.
 *
 * **Node.js only.** This class will throw at construction time in browser or
 * non-worker-threads environments.
 *
 * @example
 * ```typescript
 * import { PoolManagerSync } from "@alibaba-group/opensandbox";
 *
 * const manager = PoolManagerSync.create();
 *
 * // All calls are synchronous/blocking:
 * const pool = manager.createPool({
 *   name: "my-pool",
 *   template: { spec: { containers: [{ name: "sbx", image: "python:3.11" }] } },
 *   capacitySpec: { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0 },
 * });
 * console.log(pool.name);
 *
 * const pools = manager.listPools();
 * console.log(pools.items.length);
 *
 * manager.close();
 * ```
 */

import { fileURLToPath } from "url";
import { createRequire } from "module";
import path from "path";

import type { PoolManagerOptions } from "./poolManager.js";
import { runPoolOpSync } from "./sync/runSync.js";
import type {
  CreatePoolRequest,
  PoolInfo,
  PoolListResponse,
  UpdatePoolRequest,
} from "./models/pools.js";

// ---------------------------------------------------------------------------
// Module-path resolution
// ---------------------------------------------------------------------------

/**
 * Resolve the absolute path to the compiled `poolManager.js` so the worker
 * can `import()` it.  Works for both ESM (import.meta.url) and CJS
 * (__filename).
 */
function resolvePoolManagerPath(): string {
  try {
    // ESM: __filename-equivalent
    const __filename = fileURLToPath(import.meta.url);
    const __dirname = path.dirname(__filename);
    return path.join(__dirname, "poolManager.js");
  } catch {
    // CJS fallback
    const _require = createRequire(import.meta.url);
    return _require.resolve("./poolManager.js");
  }
}

// ---------------------------------------------------------------------------
// PoolManagerSync
// ---------------------------------------------------------------------------

/**
 * Synchronous (blocking) interface for managing pre-warmed sandbox resource
 * pools.  Every method blocks until the underlying HTTP call completes.
 *
 * Mirrors the async {@link PoolManager} API but without `await`.
 *
 * **Node.js ≥ 18 required** (`worker_threads` + `SharedArrayBuffer`).
 */
export class PoolManagerSync {
  private readonly _opts: PoolManagerOptions;
  private readonly _sdkModulePath: string;
  private _closed = false;

  private constructor(opts: PoolManagerOptions, sdkModulePath: string) {
    this._opts = opts;
    this._sdkModulePath = sdkModulePath;
  }

  /**
   * Create a `PoolManagerSync` with the provided (or default) options.
   *
   * @param opts - Connection options forwarded to `PoolManager.create()`.
   */
  static create(opts: PoolManagerOptions = {}): PoolManagerSync {
    const sdkModulePath = resolvePoolManagerPath();
    return new PoolManagerSync(opts, sdkModulePath);
  }

  // --------------------------------------------------------------------------
  // Internal helper
  // --------------------------------------------------------------------------

  private _run(payload: Parameters<typeof runPoolOpSync>[2]): unknown {
    if (this._closed) {
      throw new Error("PoolManagerSync has been closed.");
    }
    return runPoolOpSync(this._sdkModulePath, this._opts, payload);
  }

  // --------------------------------------------------------------------------
  // Pool CRUD (synchronous)
  // --------------------------------------------------------------------------

  /**
   * Create a new pre-warmed resource pool (blocking).
   *
   * @param req - Pool creation parameters.
   * @returns The newly created pool.
   * @throws {@link SandboxApiException} on server errors.
   */
  createPool(req: CreatePoolRequest): PoolInfo {
    return this._run({ op: "createPool", req }) as PoolInfo;
  }

  /**
   * Retrieve a pool by name (blocking).
   *
   * @param poolName - Name of the pool to look up.
   * @returns Current pool state including observed runtime status.
   * @throws {@link SandboxApiException} with status 404 if not found.
   */
  getPool(poolName: string): PoolInfo {
    return this._run({ op: "getPool", poolName }) as PoolInfo;
  }

  /**
   * List all pools in the namespace (blocking).
   *
   * @returns All pools.
   */
  listPools(): PoolListResponse {
    return this._run({ op: "listPools" }) as PoolListResponse;
  }

  /**
   * Update the capacity configuration of an existing pool (blocking).
   *
   * @param poolName - Name of the pool to update.
   * @param req - New capacity configuration.
   * @returns Updated pool state.
   * @throws {@link SandboxApiException} with status 404 if not found.
   */
  updatePool(poolName: string, req: UpdatePoolRequest): PoolInfo {
    return this._run({ op: "updatePool", poolName, req }) as PoolInfo;
  }

  /**
   * Delete a pool (blocking).
   *
   * @param poolName - Name of the pool to delete.
   * @throws {@link SandboxApiException} with status 404 if not found.
   */
  deletePool(poolName: string): void {
    this._run({ op: "deletePool", poolName });
  }

  // --------------------------------------------------------------------------
  // Lifecycle
  // --------------------------------------------------------------------------

  /**
   * Mark this manager as closed.
   *
   * Because each method invocation creates its own short-lived `PoolManager`
   * inside a worker thread (and disposes it after the call), there are no
   * persistent transport resources to release on the sync wrapper itself.
   * Calling `close()` prevents further calls and is useful for resource-safety
   * in `try/finally` or `using` patterns.
   */
  close(): void {
    this._closed = true;
  }

  // Disposable support (TC39 "using" / Symbol.dispose)
  [Symbol.dispose](): void {
    this.close();
  }
}
