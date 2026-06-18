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

import type {
  CreatePoolRequest,
  PoolInfo,
  PoolListResponse,
  UpdatePoolRequest,
} from "../models/pools.js";

/**
 * Service interface for managing pre-warmed sandbox resource pools.
 *
 * Abstracting over the concrete adapter implementation keeps PoolManager
 * and tests decoupled from HTTP transport details.
 */
export interface Pools {
  /**
   * Create a new pre-warmed resource pool.
   *
   * @param req - Pool creation parameters.
   * @returns The newly created pool.
   * @throws {@link SandboxApiException} on server errors.
   */
  createPool(req: CreatePoolRequest): Promise<PoolInfo>;

  /**
   * Retrieve a pool by name.
   *
   * @param poolName - Name of the pool.
   * @returns Current pool state including observed runtime status.
   * @throws {@link SandboxApiException} with status 404 if not found.
   */
  getPool(poolName: string): Promise<PoolInfo>;

  /**
   * List all pools in the namespace.
   *
   * @returns All pools.
   */
  listPools(): Promise<PoolListResponse>;

  /**
   * Update the capacity configuration of an existing pool.
   *
   * @param poolName - Name of the pool to update.
   * @param req - New capacity configuration.
   * @returns Updated pool state.
   * @throws {@link SandboxApiException} with status 404 if not found.
   */
  updatePool(poolName: string, req: UpdatePoolRequest): Promise<PoolInfo>;

  /**
   * Delete a pool.
   *
   * @param poolName - Name of the pool to delete.
   * @throws {@link SandboxApiException} with status 404 if not found.
   */
  deletePool(poolName: string): Promise<void>;
}
