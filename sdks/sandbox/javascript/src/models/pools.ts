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
 * Domain models for pre-warmed sandbox resource pools.
 *
 * Pools are Kubernetes CRD resources that keep a set of pods pre-warmed,
 * reducing sandbox cold-start latency.
 *
 * IMPORTANT:
 * - These are NOT OpenAPI-generated types.
 * - They are intentionally stable and JS-friendly.
 */

/**
 * Capacity configuration that controls how many pods are kept warm
 * and the overall pool size limits.
 */
export interface PoolCapacitySpec {
  /** Maximum number of pods kept in the warm buffer. */
  bufferMax: number;
  /** Minimum number of pods that must remain in the warm buffer. */
  bufferMin: number;
  /** Maximum total number of pods allowed in the entire pool. */
  poolMax: number;
  /** Minimum total size of the pool. */
  poolMin: number;
}

/**
 * Observed runtime state of a pool, reported by the Kubernetes controller.
 */
export interface PoolStatus {
  /** Total number of pods in the pool (warm + allocated). */
  total: number;
  /** Number of pods currently allocated to running sandboxes. */
  allocated: number;
  /** Number of pods currently available in the warm buffer. */
  available: number;
  /** Latest revision identifier of the pool spec. */
  revision: string;
}

/**
 * Full representation of a Pool resource.
 *
 * Returned by create / get / update operations and as items in list responses.
 */
export interface PoolInfo {
  /** Unique pool name (Kubernetes resource name). */
  name: string;
  /** Capacity configuration of the pool. */
  capacitySpec: PoolCapacitySpec;
  /**
   * Observed runtime state of the pool.
   * May be undefined if the controller has not yet reconciled the pool.
   */
  status?: PoolStatus;
  /** Pool creation timestamp. */
  createdAt?: Date;
}

/**
 * Response returned by the list pools endpoint.
 */
export interface PoolListResponse {
  /** All pools in the namespace. */
  items: PoolInfo[];
}

/**
 * Request body for creating a new Pool.
 */
export interface CreatePoolRequest {
  /**
   * Unique name for the pool.
   * Must be a valid Kubernetes resource name: lowercase alphanumeric and hyphens,
   * starting and ending with alphanumeric characters.
   */
  name: string;
  /**
   * Kubernetes PodTemplateSpec defining the pod configuration for pre-warmed pods.
   * Follows the same schema as `spec.template` in a Kubernetes Deployment.
   */
  template: Record<string, unknown>;
  /** Initial capacity configuration. */
  capacitySpec: PoolCapacitySpec;
}

/**
 * Request body for updating an existing Pool's capacity.
 *
 * Only `capacitySpec` can be changed after pool creation.
 * To change the pod template, delete and recreate the pool.
 */
export interface UpdatePoolRequest {
  /** New capacity configuration for the pool. */
  capacitySpec: PoolCapacitySpec;
}
