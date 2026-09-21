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

import { ConnectionConfig } from "./config/connection.js";
import {
  PoolAcquireFailedException,
  PoolDestroyedException,
  PoolEmptyException,
  PoolNotRunningException,
  PoolStateStoreUnavailableException,
  SandboxReadyTimeoutException,
} from "./core/exceptions.js";
import { subscribeAbort } from "./internal/abort.js";
import { ReadinessBudget } from "./internal/readiness.js";
import { PoolTracer, POOL_WARMUP_SPANS } from "./internal/poolTracing.js";
import { InMemoryPoolStateStore } from "./poolStore.js";
import {
  AcquirePolicy,
  PoolLifecycleState,
  PoolDestroyState,
  PoolState,
  PooledSandboxCreateReason,
  type IdleEntry,
  type PoolCreationSpec,
  type PoolHealthCheck,
  type PoolSnapshot,
  type PoolStateStore,
  type SandboxAcquireOptions,
  type SandboxPoolOptions,
  type TakeIdleResult,
} from "./poolTypes.js";
import { Sandbox } from "./sandbox.js";
import { SandboxManager } from "./manager.js";

const DEFAULT_IDLE_TIMEOUT_SECONDS = 24 * 60 * 60;
const DEFAULT_READY_TIMEOUT_SECONDS = 30;
const DEFAULT_POLLING_INTERVAL_MILLIS = 200;
const CREATE_CONCURRENCY_HEADROOM = 1.5;

interface ResolvedPoolOptions {
  poolName: string;
  maxIdle: number;
  connectionConfig: ConnectionConfig;
  creationSpec: PoolCreationSpec;
  sandboxCreator?: SandboxPoolOptions["sandboxCreator"];
  stateStore: PoolStateStore;
  ownerId: string;
  warmupCreateQps: number;
  warmupConcurrency: number;
  primaryLockTtlSeconds: number;
  degradedThreshold: number;
  idleTimeoutSeconds: number;
  drainTimeoutSeconds: number;
  acquireMinRemainingTtlSeconds: number;
  acquireReadyTimeoutSeconds: number;
  acquireHealthCheckPollingIntervalMillis: number;
  acquireHealthCheck?: PoolHealthCheck;
  acquireSkipHealthCheck: boolean;
  maxAcquireRetries: number;
  warmupReadyTimeoutSeconds: number;
  warmupHealthCheckInitialDelayMillis: number;
  warmupHealthCheckPollingIntervalMillis: number;
  warmupHealthCheck?: PoolHealthCheck;
  warmupSandboxPreparer?: SandboxPoolOptions["warmupSandboxPreparer"];
  warmupPostPrepareHealthCheck?: PoolHealthCheck;
  warmupPostPrepareHealthCheckTimeoutSeconds: number;
  warmupSkipHealthCheck: boolean;
  logger?: SandboxPoolOptions["logger"];
}

interface AsyncSemaphoreWaiter {
  readonly ready: () => void;
  readonly reject: (reason?: unknown) => void;
  readonly signal?: AbortSignal;
}

interface AsyncSemaphoreLease {
  release?: () => void;
}

class AsyncSemaphore {
  private active = 0;
  private readonly waiters: AsyncSemaphoreWaiter[] = [];
  private readonly abortGroups = new Map<AbortSignal, {
    readonly waiters: Set<AsyncSemaphoreWaiter>;
    readonly onAbort: () => void;
  }>();

  constructor(private readonly limit: number) {}

  async acquire(signal?: AbortSignal): Promise<() => void> {
    signal?.throwIfAborted();
    if (this.active < this.limit) {
      this.active += 1;
      return () => this.release();
    }
    await new Promise<void>((resolve, reject) => {
      const waiter = {
        ready: () => {
          this.detachWaiter(waiter);
          this.active += 1;
          resolve();
        },
        reject,
        signal,
      };
      this.waiters.push(waiter);
      if (signal) {
        let group = this.abortGroups.get(signal);
        if (!group) {
          const waiters = new Set<AsyncSemaphoreWaiter>();
          const onAbort = () => {
            for (const abortedWaiter of waiters) {
              const index = this.waiters.indexOf(abortedWaiter);
              if (index >= 0) this.waiters.splice(index, 1);
              abortedWaiter.reject(signal.reason);
            }
            waiters.clear();
            this.abortGroups.delete(signal);
          };
          group = { waiters, onAbort };
          this.abortGroups.set(signal, group);
          signal.addEventListener("abort", onAbort, { once: true });
        }
        group.waiters.add(waiter);
      }
    });
    return () => this.release();
  }

  private detachWaiter(waiter: AsyncSemaphoreWaiter): void {
    const signal = waiter.signal;
    if (!signal) return;
    const group = this.abortGroups.get(signal);
    if (!group) return;
    group.waiters.delete(waiter);
    if (group.waiters.size === 0) {
      signal.removeEventListener("abort", group.onAbort);
      this.abortGroups.delete(signal);
    }
  }

  private release(): void {
    this.active -= 1;
    this.waiters.shift()?.ready();
  }
}

function makeOwnerId(): string {
  const random = globalThis.crypto?.randomUUID?.() ?? Math.random().toString(16).slice(2);
  return `pool-owner-${random}`;
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

function requireNonNegative(value: number, name: string): void {
  if (!Number.isFinite(value) || value < 0) throw new Error(`${name} must be a non-negative number`);
}

function requirePositive(value: number, name: string): void {
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${name} must be a positive number`);
}

function requireNonNegativeInteger(value: number, name: string): void {
  requireNonNegative(value, name);
  if (!Number.isInteger(value)) throw new Error(`${name} must be an integer`);
}

function policyRetries(policy: AcquirePolicy): boolean {
  return policy === AcquirePolicy.RETRY_NEXT_IDLE || policy === AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE;
}

function policyCreates(policy: AcquirePolicy): boolean {
  return policy === AcquirePolicy.DIRECT_CREATE || policy === AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE;
}

function sleep(milliseconds: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted();
  return new Promise((resolve, reject) => {
    const onDone = () => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    };
    const timer = setTimeout(onDone, milliseconds);
    if (!signal) return;
    const onAbort = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", onAbort);
      reject(signal.reason);
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

/** A client-side buffer of clean, ready sandboxes. */
export class SandboxPool {
  private readonly options: ResolvedPoolOptions;
  private manager?: SandboxManager;
  private lifecycleState = PoolLifecycleState.NOT_STARTED;
  private operatingState = PoolState.HEALTHY;
  private failureCount = 0;
  private lastError?: string;
  private inFlightOperations = 0;
  private timer?: ReturnType<typeof setInterval>;
  private heartbeatTimer?: ReturnType<typeof setInterval>;
  private startPromise?: Promise<void>;
  private reconcilePromise?: Promise<void>;
  private shutdownPromise?: Promise<void>;
  private abortController?: AbortController;
  private allowWarmupCommitWhileDraining = false;
  private runGeneration = 0;
  private leaderEpoch = 0;
  private primaryOwned = false;
  private readonly warmupTasks = new Map<Promise<void>, number>();
  private createSemaphore: AsyncSemaphore;
  private postCreateSemaphore: AsyncSemaphore;
  private readonly tracer: PoolTracer;

  private constructor(options: SandboxPoolOptions) {
    const poolName = options.poolName?.trim();
    if (!poolName) throw new Error("poolName is required");
    if (options.ownerId !== undefined && !options.ownerId.trim()) {
      throw new Error("ownerId must not be blank");
    }
    requireNonNegativeInteger(options.maxIdle, "maxIdle");

    const idleTimeoutSeconds = options.idleTimeoutSeconds ?? DEFAULT_IDLE_TIMEOUT_SECONDS;
    const acquireMinRemainingTtlSeconds =
      options.acquireMinRemainingTtlSeconds ?? Math.min(idleTimeoutSeconds / 2, 60);
    const warmupConcurrency = options.warmupConcurrency ?? 128;
    const creationSpec = options.creationSpec ?? ({} as PoolCreationSpec);
    if (
      !options.sandboxCreator &&
      (creationSpec.image === undefined) === (creationSpec.snapshotId === undefined)
    ) {
      throw new Error("creationSpec must provide exactly one of image or snapshotId when sandboxCreator is absent");
    }

    requirePositive(idleTimeoutSeconds, "idleTimeoutSeconds");
    requirePositive(warmupConcurrency, "warmupConcurrency");
    if (!Number.isInteger(warmupConcurrency)) throw new Error("warmupConcurrency must be an integer");
    requirePositive(options.warmupCreateQps ?? 10, "warmupCreateQps");
    if (!Number.isInteger(options.warmupCreateQps ?? 10)) {
      throw new Error("warmupCreateQps must be an integer");
    }
    requirePositive(options.primaryLockTtlSeconds ?? 60, "primaryLockTtlSeconds");
    requirePositive(options.degradedThreshold ?? 3, "degradedThreshold");
    if (!Number.isInteger(options.degradedThreshold ?? 3)) {
      throw new Error("degradedThreshold must be an integer");
    }
    requireNonNegative(options.drainTimeoutSeconds ?? 30, "drainTimeoutSeconds");
    requireNonNegative(acquireMinRemainingTtlSeconds, "acquireMinRemainingTtlSeconds");
    if (acquireMinRemainingTtlSeconds >= idleTimeoutSeconds) {
      throw new Error("acquireMinRemainingTtlSeconds must be less than idleTimeoutSeconds");
    }
    requirePositive(options.acquireReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS, "acquireReadyTimeoutSeconds");
    requirePositive(options.warmupReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS, "warmupReadyTimeoutSeconds");
    requireNonNegative(options.warmupHealthCheckInitialDelayMillis ?? 0, "warmupHealthCheckInitialDelayMillis");
    requirePositive(
      options.acquireHealthCheckPollingIntervalMillis ?? DEFAULT_POLLING_INTERVAL_MILLIS,
      "acquireHealthCheckPollingIntervalMillis",
    );
    requirePositive(
      options.warmupHealthCheckPollingIntervalMillis ?? 500,
      "warmupHealthCheckPollingIntervalMillis",
    );
    requirePositive(options.maxAcquireRetries ?? 3, "maxAcquireRetries");
    if (!Number.isInteger(options.maxAcquireRetries ?? 3)) {
      throw new Error("maxAcquireRetries must be an integer");
    }
    requirePositive(
      options.warmupPostPrepareHealthCheckTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
      "warmupPostPrepareHealthCheckTimeoutSeconds",
    );

    this.options = {
      poolName,
      maxIdle: options.maxIdle,
      connectionConfig:
        options.connectionConfig instanceof ConnectionConfig
          ? cloneConnectionConfig(options.connectionConfig)
          : new ConnectionConfig(options.connectionConfig),
      creationSpec,
      sandboxCreator: options.sandboxCreator,
      stateStore: options.stateStore ?? new InMemoryPoolStateStore(),
      ownerId: options.ownerId?.trim() ?? makeOwnerId(),
      warmupCreateQps: options.warmupCreateQps ?? 10,
      warmupConcurrency,
      primaryLockTtlSeconds: options.primaryLockTtlSeconds ?? 60,
      degradedThreshold: options.degradedThreshold ?? 3,
      idleTimeoutSeconds,
      drainTimeoutSeconds: options.drainTimeoutSeconds ?? 30,
      acquireMinRemainingTtlSeconds,
      acquireReadyTimeoutSeconds: options.acquireReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
      acquireHealthCheckPollingIntervalMillis:
        options.acquireHealthCheckPollingIntervalMillis ?? DEFAULT_POLLING_INTERVAL_MILLIS,
      acquireHealthCheck: options.acquireHealthCheck,
      acquireSkipHealthCheck: options.acquireSkipHealthCheck ?? false,
      maxAcquireRetries: options.maxAcquireRetries ?? 3,
      warmupReadyTimeoutSeconds: options.warmupReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
      warmupHealthCheckInitialDelayMillis: options.warmupHealthCheckInitialDelayMillis ?? 0,
      warmupHealthCheckPollingIntervalMillis:
        options.warmupHealthCheckPollingIntervalMillis ?? 500,
      warmupHealthCheck: options.warmupHealthCheck,
      warmupSandboxPreparer: options.warmupSandboxPreparer,
      warmupPostPrepareHealthCheck: options.warmupPostPrepareHealthCheck,
      warmupPostPrepareHealthCheckTimeoutSeconds:
        options.warmupPostPrepareHealthCheckTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
      warmupSkipHealthCheck: options.warmupSkipHealthCheck ?? false,
      logger: options.logger,
    };
    this.createSemaphore = new AsyncSemaphore(Math.ceil(this.options.warmupCreateQps * CREATE_CONCURRENCY_HEADROOM));
    this.postCreateSemaphore = new AsyncSemaphore(warmupConcurrency);
    this.tracer = PoolTracer.from(this.options.connectionConfig);
  }

  static create(options: SandboxPoolOptions): SandboxPool {
    return new SandboxPool(options);
  }

  static inMemoryStateStore(): InMemoryPoolStateStore {
    return new InMemoryPoolStateStore();
  }

  async start(): Promise<void> {
    if (this.lifecycleState === PoolLifecycleState.RUNNING) return;
    if (this.lifecycleState === PoolLifecycleState.STARTING) {
      await this.startPromise;
      return;
    }
    if (this.lifecycleState === PoolLifecycleState.DRAINING) {
      throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
    }
    this.lifecycleState = PoolLifecycleState.STARTING;
    this.startPromise = (async () => {
      await this.reconcilePromise?.catch(() => undefined);
      await this.finishStart();
    })();
    try {
      await this.startPromise;
    } finally {
      this.startPromise = undefined;
    }
  }

  private async finishStart(): Promise<void> {
    try {
      await this.ensurePoolNamespaceActive();
      await this.options.stateStore.setMaxIdle(this.options.poolName, this.options.maxIdle);
      await this.options.stateStore.setIdleEntryTtl(this.options.poolName, this.options.idleTimeoutSeconds);
    } catch (cause) {
      if (this.lifecycleState === PoolLifecycleState.STARTING) {
        this.lifecycleState = PoolLifecycleState.STOPPED;
      }
      if (cause instanceof PoolDestroyedException || cause instanceof PoolStateStoreUnavailableException) throw cause;
      throw new PoolStateStoreUnavailableException("start", cause);
    }
    if (this.lifecycleState !== PoolLifecycleState.STARTING) {
      throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
    }

    this.abortController = new AbortController();
    this.runGeneration += 1;
    this.createSemaphore = new AsyncSemaphore(
      Math.ceil(this.options.warmupCreateQps * CREATE_CONCURRENCY_HEADROOM),
    );
    this.postCreateSemaphore = new AsyncSemaphore(this.options.warmupConcurrency);
    try {
      this.manager ??= this.createManager();
    } catch (cause) {
      if (this.lifecycleState === PoolLifecycleState.STARTING) {
        this.lifecycleState = PoolLifecycleState.STOPPED;
      }
      throw cause;
    }
    this.recordSuccess();
    this.lifecycleState = PoolLifecycleState.RUNNING;
    this.timer = setInterval(() => this.triggerReconcile(), 1_000);
    (this.timer as unknown as { unref?: () => void }).unref?.();
    const heartbeatIntervalMillis = Math.max(
      1,
      Math.min(1_000, Math.floor((this.options.primaryLockTtlSeconds * 1_000) / 3)),
    );
    const heartbeatGeneration = this.runGeneration;
    this.heartbeatTimer = setInterval(
      () => void this.runPrimaryHeartbeat(heartbeatGeneration),
      heartbeatIntervalMillis,
    );
    (this.heartbeatTimer as unknown as { unref?: () => void }).unref?.();
    if (this.options.primaryLockTtlSeconds <= this.options.warmupReadyTimeoutSeconds) {
      this.options.logger?.warn?.("pool primary lock TTL may expire during warmup", {
        poolName: this.options.poolName,
        primaryLockTtlSeconds: this.options.primaryLockTtlSeconds,
        warmupReadyTimeoutSeconds: this.options.warmupReadyTimeoutSeconds,
      });
    }
    if (this.options.maxIdle > 0) this.triggerReconcile();
  }

  async acquire(options: SandboxAcquireOptions = {}): Promise<Sandbox> {
    if (this.lifecycleState !== PoolLifecycleState.RUNNING) {
      await this.throwIfPoolNamespaceDestroyed();
      throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
    }
    options.signal?.throwIfAborted();
    const policy = options.policy ?? AcquirePolicy.DIRECT_CREATE;
    await this.ensurePoolNamespaceActiveForAcquire(policy);
    const generation = this.runGeneration;
    const maxAttempts = policyRetries(policy) ? this.options.maxAcquireRetries : 1;
    const minTtl = options.minRemainingTtlSeconds ?? this.options.acquireMinRemainingTtlSeconds;
    requireNonNegative(minTtl, "minRemainingTtlSeconds");
    if (options.sandboxTimeoutSeconds !== undefined) {
      requirePositive(options.sandboxTimeoutSeconds, "sandboxTimeoutSeconds");
    }

    this.inFlightOperations += 1;
    try {
      let attempted = false;
      let lastError: unknown;
      for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
        let result: TakeIdleResult;
        try {
          result = await this.options.stateStore.tryTakeIdleWithMinTtl(this.options.poolName, minTtl);
        } catch (cause) {
          if (cause instanceof PoolDestroyedException) throw cause;
          if (!policyCreates(policy)) throw new PoolStateStoreUnavailableException("tryTakeIdle", cause);
          this.options.logger?.warn?.("pool state store unavailable; falling back to direct create", {
            poolName: this.options.poolName,
            error: cause,
          });
          lastError = cause;
          break;
        }
        void this.killSandboxIds(result.discardedAliveSandboxIds).catch(() => undefined);
        if (!result.sandboxId) break;
        attempted = true;
        let sandbox: Sandbox | undefined;
        try {
          sandbox = await Sandbox.connect({
            sandboxId: result.sandboxId,
            connectionConfig: this.options.connectionConfig,
            adapterFactory: this.options.creationSpec.adapterFactory,
            skipHealthCheck: true,
            signal: options.signal,
          });
          if (!(options.skipHealthCheck ?? this.options.acquireSkipHealthCheck)) {
            await this.waitUntilHealthy(
              sandbox,
              this.options.acquireHealthCheck,
              this.options.acquireReadyTimeoutSeconds,
              this.options.acquireHealthCheckPollingIntervalMillis,
              options.signal,
            );
          }
        } catch (cause) {
          lastError = cause;
          await sandbox?.close().catch(() => undefined);
          void this.killSandboxId(result.sandboxId).catch(() => undefined);
          if (options.signal?.aborted) throw options.signal.reason;
          this.options.logger?.warn?.("pool idle sandbox connect or health check failed", {
            poolName: this.options.poolName,
            sandboxId: result.sandboxId,
            attempt: attempt + 1,
            error: cause,
          });
          await this.ensureAcquireStillRunning(generation);
          await this.ensurePoolNamespaceActive();
          if (!policyRetries(policy)) break;
          continue;
        }
        try {
          await this.renewAcquired(sandbox, options.sandboxTimeoutSeconds);
          await this.ensureAcquireStillRunning(generation);
        } catch (cause) {
          await this.killAndClose(sandbox);
          throw cause;
        }
        return sandbox;
      }

      await this.ensureAcquireStillRunning(generation);
      if (!policyCreates(policy)) {
        if (attempted) throw new PoolAcquireFailedException(this.options.poolName, lastError);
        throw new PoolEmptyException(this.options.poolName);
      }

      await this.ensurePoolNamespaceActiveForAcquire(policy);
      const sandbox = await this.createSandbox(PooledSandboxCreateReason.DIRECT_CREATE, options.signal);
      try {
        if (!(options.skipHealthCheck ?? this.options.acquireSkipHealthCheck)) {
          await this.waitUntilHealthy(
            sandbox,
            this.options.acquireHealthCheck,
            this.options.acquireReadyTimeoutSeconds,
            this.options.acquireHealthCheckPollingIntervalMillis,
            options.signal,
          );
        }
        await this.renewAcquired(sandbox, options.sandboxTimeoutSeconds);
        await this.ensurePoolNamespaceActiveForAcquire(policy);
        await this.ensureAcquireStillRunning(generation);
        return sandbox;
      } catch (cause) {
        await this.killAndClose(sandbox);
        throw cause;
      }
    } finally {
      this.inFlightOperations -= 1;
    }
  }

  async resize(maxIdle: number): Promise<void> {
    requireNonNegativeInteger(maxIdle, "maxIdle");
    await this.ensurePoolNamespaceActive();
    this.inFlightOperations += 1;
    try {
      await this.storeCall("setMaxIdle", () => this.options.stateStore.setMaxIdle(this.options.poolName, maxIdle));
      this.options.maxIdle = maxIdle;
    } finally {
      this.inFlightOperations -= 1;
    }
  }

  async releaseAllIdle(concurrency = 1): Promise<number> {
    requirePositive(concurrency, "concurrency");
    if (!Number.isInteger(concurrency)) throw new Error("concurrency must be an integer");
    this.inFlightOperations += 1;
    try {
      const sandboxIds: string[] = [];
      let drainFailure: unknown;
      while (true) {
        try {
          const sandboxId = await this.storeCall("tryTakeIdle", () =>
            this.options.stateStore.tryTakeIdle(this.options.poolName),
          );
          if (!sandboxId) break;
          sandboxIds.push(sandboxId);
        } catch (error) {
          drainFailure = error;
          break;
        }
      }
      let next = 0;
      await Promise.all(
        Array.from({ length: Math.min(concurrency, sandboxIds.length) }, async () => {
          while (next < sandboxIds.length) {
            const sandboxId = sandboxIds[next++];
            await this.killSandboxId(sandboxId!);
          }
        }),
      );
      if (drainFailure !== undefined) throw drainFailure;
      return sandboxIds.length;
    } finally {
      this.inFlightOperations -= 1;
    }
  }

  async snapshot(): Promise<PoolSnapshot> {
    const counters = await this.storeCall("snapshotCounters", () =>
      this.options.stateStore.snapshotCounters(this.options.poolName),
    );
    return {
      lifecycleState: this.lifecycleState,
      state:
        this.lifecycleState === PoolLifecycleState.DRAINING
          ? PoolState.DRAINING
          : this.lifecycleState === PoolLifecycleState.NOT_STARTED || this.lifecycleState === PoolLifecycleState.STOPPED
            ? PoolState.STOPPED
            : this.operatingState,
      idleCount: counters.idleCount,
      maxIdle:
        (await this.storeCall("getMaxIdle", () =>
          this.options.stateStore.getMaxIdle(this.options.poolName),
        )) ?? this.options.maxIdle,
      failureCount: this.failureCount,
      backoffActive: false,
      lastError: this.lastError,
      inFlightOperations: this.inFlightOperations,
    };
  }

  snapshotIdleEntries(): Promise<IdleEntry[]> {
    return this.storeCall("snapshotIdleEntries", () =>
      this.options.stateStore.snapshotIdleEntries(this.options.poolName),
    );
  }

  async shutdown(graceful = true): Promise<void> {
    if (this.lifecycleState === PoolLifecycleState.NOT_STARTED || this.lifecycleState === PoolLifecycleState.STOPPED) {
      this.lifecycleState = PoolLifecycleState.STOPPED;
      return;
    }
    if (this.lifecycleState === PoolLifecycleState.DRAINING) {
      await this.shutdownPromise;
      return;
    }
    this.lifecycleState = PoolLifecycleState.DRAINING;
    this.allowWarmupCommitWhileDraining = graceful;
    this.shutdownPromise = this.finishShutdown(graceful);
    await this.shutdownPromise;
  }

  private async finishShutdown(graceful: boolean): Promise<void> {
    try {
      await this.startPromise?.catch(() => undefined);
      if (this.timer) clearInterval(this.timer);
      this.timer = undefined;
      if (this.heartbeatTimer) clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = undefined;

      if (graceful) {
        const deadline = Date.now() + this.options.drainTimeoutSeconds * 1000;
        while ((this.inFlightOperations > 0 || this.reconcilePromise) && Date.now() < deadline) {
          await sleep(10);
        }
      }
      this.allowWarmupCommitWhileDraining = false;
      this.abortController?.abort(new Error("Sandbox pool is shutting down"));
      try {
        await this.options.stateStore.releasePrimaryLock(this.options.poolName, this.options.ownerId);
      } catch {
        // Best-effort release; a TTL-backed lock will expire naturally.
      }
      try {
        await this.manager?.close();
      } catch {
        // Shutdown still transitions to STOPPED when transport cleanup fails.
      }
      this.manager = undefined;
      this.markPrimaryLost();
    } finally {
      this.allowWarmupCommitWhileDraining = false;
      this.lifecycleState = PoolLifecycleState.STOPPED;
      this.shutdownPromise = undefined;
    }
  }

  close(): Promise<void> {
    return this.shutdown(true);
  }

  private triggerReconcile(): void {
    if (this.lifecycleState !== PoolLifecycleState.RUNNING || this.reconcilePromise) return;
    this.reconcilePromise = this.reconcile()
      .catch((error: unknown) => {
        this.markPrimaryLost();
        this.recordFailure(error);
      })
      .finally(() => {
        this.reconcilePromise = undefined;
      });
  }

  private async reconcile(): Promise<void> {
    const { poolName, ownerId, primaryLockTtlSeconds, stateStore } = this.options;
    const destroyState = await stateStore.getDestroyState(poolName);
    if (destroyState !== PoolDestroyState.ACTIVE) {
      await this.stopAfterNamespaceDestroyed(destroyState);
      return;
    }
    if (!(await stateStore.tryAcquirePrimaryLock(poolName, ownerId, primaryLockTtlSeconds))) {
      this.markPrimaryLost();
      return;
    }
    this.markPrimaryAcquired();

    const reaped = await stateStore.reapExpiredIdleWithMinTtl(
      poolName,
      new Date(),
      this.options.acquireMinRemainingTtlSeconds,
    );
    await this.killSandboxIds(reaped.discardedAliveSandboxIds);

    const maxIdle = (await stateStore.getMaxIdle(poolName)) ?? this.options.maxIdle;
    let idleCount = (await stateStore.snapshotCounters(poolName)).idleCount;
    while (idleCount > maxIdle) {
      const sandboxId = await stateStore.tryTakeIdle(poolName);
      if (!sandboxId) break;
      await this.killSandboxId(sandboxId);
      idleCount -= 1;
    }

    const warmingCount = [...this.warmupTasks.values()].filter((value) => value === this.runGeneration).length;
    const deficit = Math.max(0, maxIdle - idleCount - warmingCount);
    const createCount = Math.min(deficit, this.options.warmupCreateQps);
    const generation = this.runGeneration;
    const leaderEpoch = this.leaderEpoch;
    const signal = this.abortController?.signal;
    const postCreateSemaphore = this.postCreateSemaphore;
    for (let index = 0; index < createCount; index += 1) {
      const taskAbortController = new AbortController();
      const unsubscribeAbort = subscribeAbort(
        signal,
        () => taskAbortController.abort(signal?.reason),
      );
      const task = this.tracer.runWarmup(
        {
          "pool.name": poolName,
          "pool.owner": ownerId,
          "pool.run.generation": generation,
          "pool.leader.epoch": this.leaderEpoch,
        },
        () => this.createIdleSandbox(
          generation,
          leaderEpoch,
          postCreateSemaphore,
          taskAbortController.signal,
        ),
      )
        .then(() => this.recordSuccess())
        .catch((error: unknown) => {
          this.recordFailure(error);
          this.options.logger?.warn?.("pool warmup sandbox creation failed", { poolName, error });
        })
        .finally(() => {
          unsubscribeAbort();
          this.warmupTasks.delete(task);
        });
      this.warmupTasks.set(task, generation);
    }
  }

  private async createIdleSandbox(
    generation: number,
    leaderEpoch: number,
    postCreateSemaphore: AsyncSemaphore,
    signal?: AbortSignal,
  ): Promise<void> {
    this.inFlightOperations += 1;
    let sandbox: Sandbox | undefined;
    let committed = false;
    try {
      const releaseCreate = await this.createSemaphore.acquire(signal);
      try {
        sandbox = await this.tracer.runPhase(POOL_WARMUP_SPANS.create, async () =>
          await this.createSandbox(PooledSandboxCreateReason.WARMUP, signal, true),
        );
      } finally {
        releaseCreate();
      }
      const warmupReadinessDeadline = performance.now() + this.options.warmupReadyTimeoutSeconds * 1_000;
      // Decide the final attempt before sleeping, as Kotlin does: the delay timer can fire slightly
      // before performance.now() reaches the deadline. Compare in seconds, because 2.007 * 1_000 is
      // 2007.0000000000002 and a millisecond comparison would miss equal settings.
      const warmupFinalAttemptOnly =
        this.options.warmupHealthCheckInitialDelayMillis / 1_000 >= this.options.warmupReadyTimeoutSeconds;
      if (!this.options.warmupSkipHealthCheck && this.options.warmupHealthCheckInitialDelayMillis > 0) {
        await sleep(
          Math.min(
            this.options.warmupHealthCheckInitialDelayMillis,
            this.options.warmupReadyTimeoutSeconds * 1_000,
          ),
          signal,
        );
      }
      const lease: AsyncSemaphoreLease = {};
      try {
        if (!this.options.warmupSkipHealthCheck) {
          await this.tracer.runPhase(POOL_WARMUP_SPANS.readiness, async () =>
            await this.waitUntilWarmupHealthy(
              sandbox!,
              warmupReadinessDeadline,
              warmupFinalAttemptOnly,
              postCreateSemaphore,
              lease,
              signal,
            ),
          );
        } else {
          lease.release = await postCreateSemaphore.acquire(signal);
        }
        if (this.options.warmupSandboxPreparer) {
          await this.tracer.runPhase(POOL_WARMUP_SPANS.prepare, async () =>
            await runAbortable(
              () => this.options.warmupSandboxPreparer!(sandbox!),
              signal,
            ),
          );
        }
        if (this.options.warmupPostPrepareHealthCheck) {
          await this.tracer.runPhase(POOL_WARMUP_SPANS.postPrepareReadiness, async () =>
            await this.waitUntilHealthyWithSemaphore(
              sandbox!,
              this.options.warmupPostPrepareHealthCheck!,
              this.options.warmupPostPrepareHealthCheckTimeoutSeconds,
              this.options.warmupHealthCheckPollingIntervalMillis,
              postCreateSemaphore,
              lease,
              signal,
            ),
          );
        }
        await this.tracer.runPhase(POOL_WARMUP_SPANS.renew, async () =>
          await sandbox!.renew(this.options.idleTimeoutSeconds),
        );
        const didCommit = await this.tracer.runPhase(POOL_WARMUP_SPANS.commit, async () => {
          const stillPrimary = await this.options.stateStore.renewPrimaryLock(
            this.options.poolName,
            this.options.ownerId,
            this.options.primaryLockTtlSeconds,
          );
          const mayCommit =
            generation === this.runGeneration &&
            leaderEpoch === this.leaderEpoch &&
            this.primaryOwned &&
            (this.lifecycleState === PoolLifecycleState.RUNNING ||
              (this.lifecycleState === PoolLifecycleState.DRAINING && this.allowWarmupCommitWhileDraining));
          if (!stillPrimary || !mayCommit) return false;
          await this.options.stateStore.putIdle(this.options.poolName, String(sandbox!.id));
          return true;
        });
        if (!didCommit) {
          await this.killAndClose(sandbox);
          return;
        }
        committed = true;
        await sandbox.close().catch((error: unknown) => {
          this.options.logger?.warn?.("failed to close pooled sandbox client", {
            sandboxId: String(sandbox!.id),
            error,
          });
        });
      } finally {
        lease.release?.();
        lease.release = undefined;
      }
    } catch (cause) {
      if (sandbox && !committed) await this.killAndClose(sandbox);
      throw cause;
    } finally {
      this.inFlightOperations -= 1;
    }
  }

  private async createSandbox(
    reason: PooledSandboxCreateReason,
    signal?: AbortSignal,
    injectTraceContext = false,
  ): Promise<Sandbox> {
    const createConnectionConfig = injectTraceContext
      ? this.tracer.createConnectionConfig(this.options.connectionConfig)
      : this.options.connectionConfig;
    if (this.options.sandboxCreator) {
      return await this.options.sandboxCreator({
        poolName: this.options.poolName,
        ownerId: this.options.ownerId,
        idleTimeoutSeconds: this.options.idleTimeoutSeconds,
        reason,
        readyTimeoutSeconds:
          reason === PooledSandboxCreateReason.WARMUP
            ? this.options.warmupReadyTimeoutSeconds
            : this.options.acquireReadyTimeoutSeconds,
        healthCheckPollingIntervalMillis:
          reason === PooledSandboxCreateReason.WARMUP
            ? this.options.warmupHealthCheckPollingIntervalMillis
            : this.options.acquireHealthCheckPollingIntervalMillis,
        skipHealthCheck: true,
        healthCheck:
          reason === PooledSandboxCreateReason.WARMUP
            ? this.options.warmupHealthCheck
            : this.options.acquireHealthCheck,
        connectionConfig: this.options.connectionConfig,
        createConnectionConfig,
        creationSpec: this.options.creationSpec,
        signal,
      });
    }
    return await Sandbox.create({
      ...this.options.creationSpec,
      connectionConfig: createConnectionConfig,
      timeoutSeconds: this.options.idleTimeoutSeconds,
      skipHealthCheck: true,
      signal,
    });
  }

  private async waitUntilHealthy(
    sandbox: Sandbox,
    healthCheck: PoolHealthCheck | undefined,
    timeoutSeconds: number,
    pollingIntervalMillis: number,
    signal?: AbortSignal,
  ): Promise<void> {
    if (typeof sandbox.waitUntilReady === "function") {
      await sandbox.waitUntilReady({
        readyTimeoutSeconds: timeoutSeconds,
        pollingIntervalMillis,
        healthCheck,
        signal,
      });
      return;
    }

    // Custom creators may return compatible objects with only isHealthy().
    const budget = new ReadinessBudget(timeoutSeconds, signal);
    budget.healthContext(`domain=${this.options.connectionConfig.domain}, useServerProxy=${this.options.connectionConfig.useServerProxy}`);
    while (true) {
      try {
        budget.attempt();
        const healthy = await budget.run(async () => healthCheck ? await healthCheck(sandbox) : await sandbox.isHealthy());
        if (healthy) return;
        budget.record("Health check returned false continuously.");
      } catch (error) {
        budget.remaining();
        budget.record(error);
      }
      await budget.pause(pollingIntervalMillis);
    }
  }

  private async waitUntilHealthyWithSemaphore(
    sandbox: Sandbox,
    healthCheck: PoolHealthCheck,
    timeoutSeconds: number,
    pollingIntervalMillis: number,
    semaphore: AsyncSemaphore,
    lease: AsyncSemaphoreLease,
    signal?: AbortSignal,
  ): Promise<void> {
    const budget = new ReadinessBudget(timeoutSeconds, signal);
    budget.healthContext(`domain=${this.options.connectionConfig.domain}, useServerProxy=${this.options.connectionConfig.useServerProxy}`);
    while (true) {
      let healthy = false;
      try {
        lease.release ??= await semaphore.acquire(signal);
        budget.attempt();
        healthy = await budget.run(async () => await healthCheck(sandbox));
        if (healthy) return;
        budget.record("Health check returned false continuously.");
      } catch (error) {
        budget.remaining();
        budget.record(error);
      } finally {
        if (!healthy) {
          lease.release?.();
          lease.release = undefined;
        }
      }
      await budget.pause(pollingIntervalMillis);
    }
  }

  private async waitUntilWarmupHealthy(
    sandbox: Sandbox,
    deadline: number,
    finalAttemptOnly: boolean,
    semaphore: AsyncSemaphore,
    lease: AsyncSemaphoreLease,
    signal?: AbortSignal,
  ): Promise<void> {
    const remainingMillis = deadline - performance.now();
    if (!finalAttemptOnly && remainingMillis > 0) {
      await this.waitUntilHealthyWithSemaphore(
        sandbox,
        this.options.warmupHealthCheck ?? ((current) => current.isHealthy()),
        remainingMillis / 1_000,
        this.options.warmupHealthCheckPollingIntervalMillis,
        semaphore,
        lease,
        signal,
      );
      return;
    }

    signal?.throwIfAborted();
    lease.release ??= await semaphore.acquire(signal);
    const healthy = await runAbortable(() => this.options.warmupHealthCheck
      ? this.options.warmupHealthCheck(sandbox)
      : sandbox.isHealthy(), signal);
    if (!healthy) {
      throw new SandboxReadyTimeoutException({
        message: `Sandbox warmup readiness timed out after ${this.options.warmupReadyTimeoutSeconds}s`,
      });
    }
  }

  private async renewAcquired(sandbox: Sandbox, timeoutSeconds?: number): Promise<void> {
    if (timeoutSeconds === undefined) return;
    await sandbox.renew(timeoutSeconds);
  }

  private async killSandboxIds(sandboxIds: string[]): Promise<void> {
    if (sandboxIds.length === 0) return;
    this.inFlightOperations += 1;
    try {
      const pooledManager = this.manager;
      let manager: SandboxManager;
      try {
        manager = pooledManager ?? this.createManager();
      } catch (error) {
        this.options.logger?.warn?.("failed to create manager for pooled sandbox cleanup", { error });
        return;
      }
      try {
        for (const sandboxId of sandboxIds) {
          try {
            await manager.killSandbox(sandboxId);
          } catch (error) {
            this.options.logger?.warn?.("failed to kill pooled sandbox", { sandboxId, error });
          }
        }
      } finally {
        if (!pooledManager) await manager.close().catch(() => undefined);
      }
    } finally {
      this.inFlightOperations -= 1;
    }
  }

  private async killSandboxId(sandboxId: string): Promise<void> {
    await this.killSandboxIds([sandboxId]);
  }

  private async killAndClose(sandbox: Sandbox): Promise<void> {
    await sandbox.kill().catch(() => undefined);
    await sandbox.close().catch(() => undefined);
  }

  private async ensureAcquireStillRunning(generation: number): Promise<void> {
    if (this.lifecycleState === PoolLifecycleState.RUNNING && generation === this.runGeneration) return;
    throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
  }

  private async ensurePoolNamespaceActive(): Promise<void> {
    const state = await this.storeCall("getDestroyState", () =>
      this.options.stateStore.getDestroyState(this.options.poolName),
    );
    if (state !== PoolDestroyState.ACTIVE) {
      throw new PoolDestroyedException(this.options.poolName, state);
    }
  }

  private async throwIfPoolNamespaceDestroyed(): Promise<void> {
    try {
      await this.ensurePoolNamespaceActive();
    } catch (error) {
      if (error instanceof PoolDestroyedException) throw error;
      // A stopped local pool remains observably stopped when the shared store
      // cannot answer the stronger destroy-state question.
    }
  }

  private async ensurePoolNamespaceActiveForAcquire(policy: AcquirePolicy): Promise<void> {
    try {
      await this.ensurePoolNamespaceActive();
    } catch (error) {
      if (error instanceof PoolDestroyedException) throw error;
      if (!policyCreates(policy)) throw error;
      this.options.logger?.warn?.(
        "pool state store unavailable during destroy-state check; falling back to direct create",
        { poolName: this.options.poolName, policy, error },
      );
    }
  }

  private async runPrimaryHeartbeat(generation: number): Promise<void> {
    if (
      generation !== this.runGeneration ||
      !this.primaryOwned ||
      (this.lifecycleState !== PoolLifecycleState.RUNNING &&
        this.lifecycleState !== PoolLifecycleState.DRAINING)
    ) return;
    try {
      const renewed = await this.options.stateStore.renewPrimaryLock(
        this.options.poolName,
        this.options.ownerId,
        this.options.primaryLockTtlSeconds,
      );
      if (generation !== this.runGeneration) return;
      if (!renewed) this.markPrimaryLost();
    } catch (error) {
      this.options.logger?.warn?.("pool primary heartbeat failed", {
        poolName: this.options.poolName,
        error,
      });
    }
  }

  private async stopAfterNamespaceDestroyed(state: PoolDestroyState): Promise<void> {
    if (this.lifecycleState !== PoolLifecycleState.RUNNING) return;
    this.lifecycleState = PoolLifecycleState.STOPPED;
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
    if (this.heartbeatTimer) clearInterval(this.heartbeatTimer);
    this.heartbeatTimer = undefined;
    this.abortController?.abort(new PoolDestroyedException(this.options.poolName, state));
    try {
      await this.options.stateStore.releasePrimaryLock(this.options.poolName, this.options.ownerId);
    } catch {
      // Destroy fencing already prevents future commits; lock release is best effort.
    }
    this.markPrimaryLost();
    await this.manager?.close().catch(() => undefined);
    this.manager = undefined;
  }

  private markPrimaryAcquired(): void {
    if (this.primaryOwned) return;
    this.primaryOwned = true;
    this.leaderEpoch += 1;
  }

  private markPrimaryLost(): void {
    if (!this.primaryOwned) return;
    this.primaryOwned = false;
    this.leaderEpoch += 1;
  }

  private recordSuccess(): void {
    this.failureCount = 0;
    this.lastError = undefined;
    this.operatingState = PoolState.HEALTHY;
  }

  private recordFailure(error: unknown): void {
    this.failureCount += 1;
    this.lastError = error instanceof Error ? error.message : String(error);
    if (this.failureCount >= this.options.degradedThreshold) {
      this.operatingState = PoolState.DEGRADED;
    }
    this.options.logger?.warn?.("pool reconcile failed", {
      poolName: this.options.poolName,
      error,
      failureCount: this.failureCount,
    });
  }

  private createManager(): SandboxManager {
    return SandboxManager.create({
      connectionConfig: this.options.connectionConfig,
      adapterFactory: this.options.creationSpec.adapterFactory,
    });
  }

  private async storeCall<T>(operation: string, call: () => Promise<T>): Promise<T> {
    try {
      return await call();
    } catch (cause) {
      if (cause instanceof PoolStateStoreUnavailableException || cause instanceof PoolDestroyedException) throw cause;
      throw new PoolStateStoreUnavailableException(operation, cause);
    }
  }
}

async function runAbortable<T>(action: () => T | Promise<T>, signal?: AbortSignal): Promise<T> {
  signal?.throwIfAborted();
  if (!signal) return await action();

  let rejectOnAbort: (() => void) | undefined;
  const aborted = new Promise<never>((_, reject) => {
    rejectOnAbort = () => reject(signal.reason);
    signal.addEventListener("abort", rejectOnAbort, { once: true });
  });
  try {
    return await Promise.race([Promise.resolve().then(action), aborted]);
  } finally {
    if (rejectOnAbort) signal.removeEventListener("abort", rejectOnAbort);
  }
}
