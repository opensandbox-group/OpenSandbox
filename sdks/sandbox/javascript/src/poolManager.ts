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

import { ConnectionConfig, type ConnectionConfigOptions } from "./config/connection.js";
import { PoolsAdapter } from "./adapters/poolsAdapter.js";
import type { Pools } from "./services/pools.js";
import type {
  CreatePoolRequest,
  PoolCapacitySpec,
  PoolInfo,
  PoolListResponse,
  UpdatePoolRequest,
} from "./models/pools.js";

export interface PoolManagerOptions {
  /**
   * Connection configuration for calling the OpenSandbox Lifecycle API.
   */
  connectionConfig?: ConnectionConfig | ConnectionConfigOptions;
}

/**
 * High-level interface for managing pre-warmed sandbox resource pools.
 *
 * Pools are Kubernetes CRD resources that keep a set of pods pre-warmed,
 * reducing sandbox cold-start latency.
 *
 * @example
 * ```typescript
 * const manager = PoolManager.create();
 *
 * // Create a pool
 * const pool = await manager.createPool({
 *   name: "my-pool",
 *   template: { spec: { containers: [{ name: "sandbox", image: "python:3.11" }] } },
 *   capacitySpec: { bufferMax: 3, bufferMin: 1, poolMax: 10, poolMin: 0 },
 * });
 *
 * // List / get / update / delete
 * const pools = await manager.listPools();
 * const info  = await manager.getPool("my-pool");
 * const updated = await manager.updatePool("my-pool", {
 *   capacitySpec: { bufferMax: 5, bufferMin: 2, poolMax: 20, poolMin: 0 },
 * });
 * await manager.deletePool("my-pool");
 *
 * await manager.close();
 * ```
 *
 * **Note**: Pool management requires the server to be configured with
 * `runtime.type = 'kubernetes'`. Non-Kubernetes deployments return
 * `SandboxApiException` with status 501.
 */
export class PoolManager {
  private readonly pools: Pools;
  private readonly connectionConfig: ConnectionConfig;

  private constructor(opts: { pools: Pools; connectionConfig: ConnectionConfig }) {
    this.pools = opts.pools;
    this.connectionConfig = opts.connectionConfig;
  }

  /**
   * Create a PoolManager with the provided (or default) connection config.
   */
  static create(opts: PoolManagerOptions = {}): PoolManager {
    const baseConfig =
      opts.connectionConfig instanceof ConnectionConfig
        ? opts.connectionConfig
        : new ConnectionConfig(opts.connectionConfig);
    const connectionConfig = baseConfig.withTransportIfMissing();

    const pools = new PoolsAdapter({
      baseUrl: connectionConfig.getBaseUrl(),
      apiKey: connectionConfig.apiKey,
      headers: connectionConfig.headers,
      fetch: connectionConfig.fetch,
    });

    return new PoolManager({ pools, connectionConfig });
  }

  // --------------------------------------------------------------------------
  // Pool CRUD
  // --------------------------------------------------------------------------

  /**
   * Create a new pre-warmed resource pool.
   *
   * @param req - Pool creation parameters.
   * @returns The newly created pool.
   */
  createPool(req: CreatePoolRequest): Promise<PoolInfo> {
    return this.pools.createPool(req);
  }

  /**
   * Retrieve a pool by name.
   *
   * @param poolName - Pool name to look up.
   * @returns Current pool state including observed runtime status.
   */
  getPool(poolName: string): Promise<PoolInfo> {
    return this.pools.getPool(poolName);
  }

  /**
   * List all pools in the namespace.
   *
   * @returns All pools.
   */
  listPools(): Promise<PoolListResponse> {
    return this.pools.listPools();
  }

  /**
   * Update the capacity configuration of an existing pool.
   *
   * Only `capacitySpec` can be changed after pool creation.
   * To change the pod template, delete and recreate the pool.
   *
   * @param poolName - Name of the pool to update.
   * @param req - New capacity configuration.
   * @returns Updated pool state.
   */
  updatePool(poolName: string, req: UpdatePoolRequest): Promise<PoolInfo> {
    return this.pools.updatePool(poolName, req);
  }

  /**
   * Delete a pool.
   *
   * @param poolName - Name of the pool to delete.
   */
  deletePool(poolName: string): Promise<void> {
    return this.pools.deletePool(poolName);
  }

  // --------------------------------------------------------------------------
  // Lifecycle
  // --------------------------------------------------------------------------

  /**
   * Release the HTTP transport resources owned by this manager.
   */
  async close(): Promise<void> {
    await this.connectionConfig.closeTransport();
  }
}
