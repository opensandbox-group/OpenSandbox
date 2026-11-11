// Copyright 2026 The OpenSandbox Authors
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
import {
  PoolDestroyIncompleteException,
  PoolDestroyedException,
} from "./core/exceptions.js";
import { SandboxManager } from "./manager.js";
import type { AdapterFactory } from "./factory/adapterFactory.js";
import {
  PoolDestroyState,
  PoolDestroyStrategy,
  type PoolDestroyOptions,
  type PoolDestroyResult,
  type PoolLogger,
  type PoolStateStore,
} from "./poolTypes.js";

const DEFAULT_TOMBSTONE_TTL_SECONDS = 7 * 24 * 60 * 60;
const DEFAULT_DRAIN_TIMEOUT_SECONDS = 30;

export interface SandboxPoolManagerOptions {
  stateStore: PoolStateStore;
  connectionConfig: ConnectionConfig | ConnectionConfigOptions;
  ownerId?: string;
  logger?: PoolLogger;
  /** Advanced transport override, primarily for custom runtimes and tests. */
  adapterFactory?: AdapterFactory;
}

function makeOwnerId(): string {
  const random = globalThis.crypto?.randomUUID?.() ?? Math.random().toString(16).slice(2);
  return `pool-manager-${random}`;
}

function cloneConnectionConfig(config: ConnectionConfig): ConnectionConfig {
  return new ConnectionConfig({
    domain: config.domain,
    protocol: config.protocol,
    apiKey: config.apiKey,
    headers: { ...config.headers },
    requestTimeoutSeconds: config.requestTimeoutSeconds,
    debug: config.debug,
    useServerProxy: config.useServerProxy,
    endpointCacheTtlMs: config.endpointCacheTtlMs,
    endpointCacheSize: config.endpointCacheSize,
    endpointCacheDisabled: config.endpointCacheDisabled,
    disableMetrics: config.disableMetrics,
    enableTracing: config.enableTracing,
  });
}

/** Administrative operations for a shared sandbox-pool namespace. */
export class SandboxPoolManager {
  private readonly stateStore: PoolStateStore;
  private readonly connectionConfig: ConnectionConfig;
  private readonly ownerId: string;
  private readonly logger?: PoolLogger;
  private readonly adapterFactory?: AdapterFactory;

  constructor(options: SandboxPoolManagerOptions) {
    if (options.ownerId !== undefined && !options.ownerId.trim()) {
      throw new Error("ownerId must not be blank");
    }
    this.stateStore = options.stateStore;
    this.connectionConfig =
      options.connectionConfig instanceof ConnectionConfig
        ? cloneConnectionConfig(options.connectionConfig)
        : new ConnectionConfig(options.connectionConfig);
    this.ownerId = options.ownerId?.trim() ?? makeOwnerId();
    this.logger = options.logger;
    this.adapterFactory = options.adapterFactory;
  }

  static create(options: SandboxPoolManagerOptions): SandboxPoolManager {
    return new SandboxPoolManager(options);
  }

  async destroy(poolName: string, options: PoolDestroyOptions = {}): Promise<PoolDestroyResult> {
    if (!poolName.trim()) throw new Error("poolName must not be blank");
    const strategy = options.strategy ?? PoolDestroyStrategy.FORCE;
    if (strategy !== PoolDestroyStrategy.FORCE) throw new Error("Only FORCE destroy is supported");
    const drainTimeoutSeconds = options.drainTimeoutSeconds ?? DEFAULT_DRAIN_TIMEOUT_SECONDS;
    const tombstoneTtlSeconds =
      options.tombstoneTtlSeconds === undefined
        ? DEFAULT_TOMBSTONE_TTL_SECONDS
        : options.tombstoneTtlSeconds;
    if (!Number.isFinite(drainTimeoutSeconds) || drainTimeoutSeconds < 0) {
      throw new Error("drainTimeoutSeconds must be non-negative");
    }
    if (tombstoneTtlSeconds != null && (!Number.isFinite(tombstoneTtlSeconds) || tombstoneTtlSeconds <= 0)) {
      throw new Error("tombstoneTtlSeconds must be positive when set");
    }

    if ((await this.stateStore.getDestroyState(poolName)) === PoolDestroyState.DESTROYED) {
      return this.destroyedResult(poolName);
    }

    const manager = SandboxManager.create({
      connectionConfig: this.connectionConfig,
      adapterFactory: this.adapterFactory,
    });
    let drainedIdleCount = 0;
    let killedIdleCount = 0;
    try {
      try {
        await this.stateStore.beginDestroy(poolName, this.ownerId);
      } catch (error) {
        if (error instanceof PoolDestroyedException) return this.destroyedResult(poolName);
        throw error;
      }
      const deadline =
        drainTimeoutSeconds > 0
          ? Date.now() + drainTimeoutSeconds * 1000
          : undefined;
      while (true) {
        this.throwIfDrainDeadlineReached(poolName, deadline);
        const sandboxId = await this.stateStore.tryTakeIdle(poolName);
        if (!sandboxId) break;
        drainedIdleCount += 1;
        try {
          await this.killSandboxWithinDeadline(
            manager,
            sandboxId,
            poolName,
            deadline,
          );
          killedIdleCount += 1;
        } catch (error) {
          if (error instanceof PoolDestroyIncompleteException) throw error;
          this.logger?.warn?.("pool destroy failed to kill idle sandbox", {
            poolName,
            sandboxId,
            error,
          });
        }
      }
      try {
        await this.stateStore.clearPoolState(poolName);
        await this.stateStore.markDestroyed(poolName, this.ownerId, tombstoneTtlSeconds);
      } catch (error) {
        throw new PoolDestroyIncompleteException(poolName, error);
      }
      return {
        poolName,
        state: PoolDestroyState.DESTROYED,
        drainedIdleCount,
        killedIdleCount,
        persistentStateCleared: true,
      };
    } finally {
      await manager.close().catch(() => undefined);
    }
  }

  private throwIfDrainDeadlineReached(
    poolName: string,
    deadline: number | undefined,
  ): void {
    if (deadline !== undefined && Date.now() >= deadline) {
      throw new PoolDestroyIncompleteException(poolName);
    }
  }

  private async killSandboxWithinDeadline(
    manager: SandboxManager,
    sandboxId: string,
    poolName: string,
    deadline: number | undefined,
  ): Promise<void> {
    if (deadline === undefined) {
      await manager.killSandbox(sandboxId);
      return;
    }

    const remainingMs = deadline - Date.now();
    if (remainingMs <= 0) throw new PoolDestroyIncompleteException(poolName);

    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<never>((_, reject) => {
      timer = setTimeout(() => {
        const error = new PoolDestroyIncompleteException(poolName);
        controller.abort(error);
        reject(error);
      }, remainingMs);
    });
    try {
      await Promise.race([
        manager.killSandbox(sandboxId, controller.signal),
        timeout,
      ]);
    } finally {
      if (timer !== undefined) clearTimeout(timer);
    }
  }

  private destroyedResult(poolName: string): PoolDestroyResult {
    return {
      poolName,
      state: PoolDestroyState.DESTROYED,
      drainedIdleCount: 0,
      killedIdleCount: 0,
      persistentStateCleared: false,
    };
  }
}
