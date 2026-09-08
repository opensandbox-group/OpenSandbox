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

import { ConnectionConfig } from "./config/connection.js";
import {
  PoolAcquireFailedException,
  PoolEmptyException,
  PoolNotRunningException,
  PoolStateStoreUnavailableException,
  SandboxReadyTimeoutException,
} from "./core/exceptions.js";
import { InMemoryPoolStateStore } from "./poolStore.js";
import {
  AcquirePolicy,
  PoolHealthState,
  PoolLifecycleState,
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

interface ResolvedPoolOptions {
  poolName: string;
  maxIdle: number;
  connectionConfig: ConnectionConfig;
  creationSpec: PoolCreationSpec;
  sandboxCreator?: SandboxPoolOptions["sandboxCreator"];
  stateStore: PoolStateStore;
  ownerId: string;
  warmupConcurrency: number;
  warmupConcurrencyConfigured: boolean;
  primaryLockTtlSeconds: number;
  reconcileIntervalSeconds: number;
  degradedThreshold: number;
  emptyBehavior: AcquirePolicy;
  idleTimeoutSeconds: number;
  drainTimeoutSeconds: number;
  acquireMinRemainingTtlSeconds: number;
  acquireReadyTimeoutSeconds: number;
  acquireHealthCheckPollingIntervalMillis: number;
  acquireHealthCheck?: PoolHealthCheck;
  maxAcquireRetries: number;
  warmupReadyTimeoutSeconds: number;
  warmupHealthCheckPollingIntervalMillis: number;
  warmupHealthCheck?: PoolHealthCheck;
  warmupPreparer?: SandboxPoolOptions["warmupPreparer"];
  logger?: SandboxPoolOptions["logger"];
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
  private healthState = PoolHealthState.HEALTHY;
  private failureCount = 0;
  private lastError?: string;
  private backoffUntilMs = 0;
  private inFlightOperations = 0;
  private timer?: ReturnType<typeof setInterval>;
  private startPromise?: Promise<void>;
  private reconcilePromise?: Promise<void>;
  private shutdownPromise?: Promise<void>;
  private abortController?: AbortController;
  private allowWarmupCommitWhileDraining = false;

  private constructor(options: SandboxPoolOptions) {
    const poolName = options.poolName?.trim();
    if (!poolName) throw new Error("poolName is required");
    requireNonNegativeInteger(options.maxIdle, "maxIdle");

    const idleTimeoutSeconds = options.idleTimeoutSeconds ?? DEFAULT_IDLE_TIMEOUT_SECONDS;
    const acquireMinRemainingTtlSeconds =
      options.acquireMinRemainingTtlSeconds ?? Math.min(idleTimeoutSeconds / 2, 60);
    const warmupConcurrency = options.warmupConcurrency ?? Math.max(1, Math.ceil(options.maxIdle * 0.2));
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
    requirePositive(options.primaryLockTtlSeconds ?? 60, "primaryLockTtlSeconds");
    requirePositive(options.reconcileIntervalSeconds ?? 30, "reconcileIntervalSeconds");
    requirePositive(options.degradedThreshold ?? 3, "degradedThreshold");
    requireNonNegative(options.drainTimeoutSeconds ?? 30, "drainTimeoutSeconds");
    requireNonNegative(acquireMinRemainingTtlSeconds, "acquireMinRemainingTtlSeconds");
    if (acquireMinRemainingTtlSeconds >= idleTimeoutSeconds) {
      throw new Error("acquireMinRemainingTtlSeconds must be less than idleTimeoutSeconds");
    }
    requirePositive(options.acquireReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS, "acquireReadyTimeoutSeconds");
    requirePositive(options.warmupReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS, "warmupReadyTimeoutSeconds");
    requirePositive(
      options.acquireHealthCheckPollingIntervalMillis ?? DEFAULT_POLLING_INTERVAL_MILLIS,
      "acquireHealthCheckPollingIntervalMillis",
    );
    requirePositive(
      options.warmupHealthCheckPollingIntervalMillis ?? DEFAULT_POLLING_INTERVAL_MILLIS,
      "warmupHealthCheckPollingIntervalMillis",
    );
    requirePositive(options.maxAcquireRetries ?? 3, "maxAcquireRetries");
    if (!Number.isInteger(options.maxAcquireRetries ?? 3)) {
      throw new Error("maxAcquireRetries must be an integer");
    }

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
      ownerId: options.ownerId?.trim() ? options.ownerId.trim() : makeOwnerId(),
      warmupConcurrency,
      warmupConcurrencyConfigured: options.warmupConcurrency !== undefined,
      primaryLockTtlSeconds: options.primaryLockTtlSeconds ?? 60,
      reconcileIntervalSeconds: options.reconcileIntervalSeconds ?? 30,
      degradedThreshold: options.degradedThreshold ?? 3,
      emptyBehavior: options.emptyBehavior ?? AcquirePolicy.DIRECT_CREATE,
      idleTimeoutSeconds,
      drainTimeoutSeconds: options.drainTimeoutSeconds ?? 30,
      acquireMinRemainingTtlSeconds,
      acquireReadyTimeoutSeconds: options.acquireReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
      acquireHealthCheckPollingIntervalMillis:
        options.acquireHealthCheckPollingIntervalMillis ?? DEFAULT_POLLING_INTERVAL_MILLIS,
      acquireHealthCheck: options.acquireHealthCheck,
      maxAcquireRetries: options.maxAcquireRetries ?? 3,
      warmupReadyTimeoutSeconds: options.warmupReadyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
      warmupHealthCheckPollingIntervalMillis:
        options.warmupHealthCheckPollingIntervalMillis ?? DEFAULT_POLLING_INTERVAL_MILLIS,
      warmupHealthCheck: options.warmupHealthCheck,
      warmupPreparer: options.warmupPreparer,
      logger: options.logger,
    };
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
      await this.options.stateStore.setMaxIdle(this.options.poolName, this.options.maxIdle);
      await this.options.stateStore.setIdleEntryTtl(this.options.poolName, this.options.idleTimeoutSeconds);
    } catch (cause) {
      if (this.lifecycleState === PoolLifecycleState.STARTING) {
        this.lifecycleState = PoolLifecycleState.STOPPED;
      }
      throw new PoolStateStoreUnavailableException("start", cause);
    }
    if (this.lifecycleState !== PoolLifecycleState.STARTING) {
      throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
    }

    this.abortController = new AbortController();
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
    this.timer = setInterval(() => this.triggerReconcile(), this.options.reconcileIntervalSeconds * 1000);
    (this.timer as unknown as { unref?: () => void }).unref?.();
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
      throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
    }
    options.signal?.throwIfAborted();
    const policy = options.policy ?? this.options.emptyBehavior;
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
          if (!options.skipHealthCheck) {
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
          if (this.lifecycleState !== PoolLifecycleState.RUNNING) {
            throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
          }
          if (!policyRetries(policy)) break;
          continue;
        }
        try {
          await this.renewAcquired(sandbox, options.sandboxTimeoutSeconds);
          this.ensureAcquireStillRunning();
        } catch (cause) {
          await this.killAndClose(sandbox);
          throw cause;
        }
        return sandbox;
      }

      if (!policyCreates(policy)) {
        if (attempted) throw new PoolAcquireFailedException(this.options.poolName, lastError);
        throw new PoolEmptyException(this.options.poolName);
      }

      const sandbox = await this.createSandbox(PooledSandboxCreateReason.DIRECT_CREATE, options.signal);
      try {
        if (!options.skipHealthCheck) {
          await this.waitUntilHealthy(
            sandbox,
            this.options.acquireHealthCheck,
            this.options.acquireReadyTimeoutSeconds,
            this.options.acquireHealthCheckPollingIntervalMillis,
            options.signal,
          );
        }
        await this.renewAcquired(sandbox, options.sandboxTimeoutSeconds);
        this.ensureAcquireStillRunning();
        return sandbox;
      } catch (cause) {
        await this.killAndClose(sandbox);
        throw cause;
      }
    } finally {
      this.inFlightOperations -= 1;
      this.triggerReconcile();
    }
  }

  async resize(maxIdle: number): Promise<void> {
    requireNonNegativeInteger(maxIdle, "maxIdle");
    this.ensureRunning();
    this.inFlightOperations += 1;
    try {
      await this.storeCall("setMaxIdle", () => this.options.stateStore.setMaxIdle(this.options.poolName, maxIdle));
      this.options.maxIdle = maxIdle;
      if (!this.options.warmupConcurrencyConfigured) {
        this.options.warmupConcurrency = Math.max(1, Math.ceil(maxIdle * 0.2));
      }
      this.triggerReconcile();
    } finally {
      this.inFlightOperations -= 1;
    }
  }

  async releaseAllIdle(): Promise<number> {
    this.inFlightOperations += 1;
    try {
      const initialIdleCount = (
        await this.storeCall("snapshotCounters", () =>
          this.options.stateStore.snapshotCounters(this.options.poolName),
        )
      ).idleCount;
      let released = 0;
      while (released < initialIdleCount) {
        const sandboxId = await this.storeCall("tryTakeIdle", () =>
          this.options.stateStore.tryTakeIdle(this.options.poolName),
        );
        if (!sandboxId) return released;
        await this.killSandboxId(sandboxId);
        released += 1;
      }
      return released;
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
      healthState: this.healthState,
      idleCount: counters.idleCount,
      maxIdle: await this.storeCall("getMaxIdle", () => this.options.stateStore.getMaxIdle(this.options.poolName)),
      failureCount: this.failureCount,
      backoffActive: Date.now() < this.backoffUntilMs,
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
    } finally {
      this.allowWarmupCommitWhileDraining = false;
      this.lifecycleState = PoolLifecycleState.STOPPED;
      this.shutdownPromise = undefined;
    }
  }

  close(): Promise<void> {
    return this.shutdown(true);
  }

  private ensureRunning(): void {
    if (this.lifecycleState !== PoolLifecycleState.RUNNING) {
      throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
    }
  }

  private triggerReconcile(): void {
    if (this.lifecycleState !== PoolLifecycleState.RUNNING || this.reconcilePromise) return;
    if (Date.now() < this.backoffUntilMs) return;
    let runAgain = false;
    this.reconcilePromise = this.reconcile()
      .then((shouldContinue) => {
        runAgain = shouldContinue;
        this.recordSuccess();
      })
      .catch((error: unknown) => this.recordFailure(error))
      .finally(() => {
        this.reconcilePromise = undefined;
        if (runAgain) this.triggerReconcile();
      });
  }

  private async reconcile(): Promise<boolean> {
    const { poolName, ownerId, primaryLockTtlSeconds, stateStore } = this.options;
    if (!(await stateStore.tryAcquirePrimaryLock(poolName, ownerId, primaryLockTtlSeconds))) return false;

    let heartbeatError: unknown;
    let leadershipLost = false;
    let heartbeatInFlight = Promise.resolve();
    const heartbeatIntervalMillis = Math.max(1, Math.floor((primaryLockTtlSeconds * 1000) / 3));
    const heartbeat = setInterval(() => {
      heartbeatInFlight = heartbeatInFlight.then(async () => {
        try {
          if (!(await stateStore.renewPrimaryLock(poolName, ownerId, primaryLockTtlSeconds))) {
            leadershipLost = true;
          }
        } catch (error) {
          heartbeatError = error;
        }
      });
    }, heartbeatIntervalMillis);
    (heartbeat as unknown as { unref?: () => void }).unref?.();

    try {
      const reaped = await stateStore.reapExpiredIdleWithMinTtl(
        poolName,
        new Date(),
        this.options.acquireMinRemainingTtlSeconds,
      );
      await this.killSandboxIds(reaped.discardedAliveSandboxIds);

      const maxIdle = await stateStore.getMaxIdle(poolName);
      let idleCount = (await stateStore.snapshotCounters(poolName)).idleCount;
      while (idleCount > maxIdle) {
        const sandboxId = await stateStore.tryTakeIdle(poolName);
        if (!sandboxId) break;
        await this.killSandboxId(sandboxId);
        idleCount -= 1;
      }

      const deficit = maxIdle - idleCount;
      const createCount = Math.min(deficit, this.options.warmupConcurrency);
      if (createCount <= 0) {
        if (!(await stateStore.renewPrimaryLock(poolName, ownerId, primaryLockTtlSeconds))) {
          leadershipLost = true;
        }
      } else {
        const results = await Promise.allSettled(
          Array.from({ length: createCount }, () => this.createIdleSandbox()),
        );
        const failures = results.filter((result): result is PromiseRejectedResult => result.status === "rejected");
        for (const failure of failures) {
          this.options.logger?.warn?.("pool warmup sandbox creation failed", {
            poolName,
            error: failure.reason,
          });
        }
        if (failures.length > 0) throw failures[0].reason;
      }
    } finally {
      clearInterval(heartbeat);
      await heartbeatInFlight;
    }
    if (heartbeatError) throw heartbeatError;
    if (leadershipLost) {
      this.options.logger?.warn?.("pool lost primary ownership during reconcile", { poolName });
      return false;
    }
    const latestMaxIdle = await stateStore.getMaxIdle(poolName);
    return (await stateStore.snapshotCounters(poolName)).idleCount < latestMaxIdle;
  }

  private async createIdleSandbox(): Promise<void> {
    this.inFlightOperations += 1;
    let sandbox: Sandbox | undefined;
    let committed = false;
    try {
      sandbox = await this.createSandbox(PooledSandboxCreateReason.WARMUP, this.abortController?.signal);
      await this.waitUntilHealthy(
        sandbox,
        this.options.warmupHealthCheck,
        this.options.warmupReadyTimeoutSeconds,
        this.options.warmupHealthCheckPollingIntervalMillis,
        this.abortController?.signal,
      );
      await this.options.warmupPreparer?.(sandbox);
      await sandbox.renew(this.options.idleTimeoutSeconds);
      const stillPrimary = await this.options.stateStore.renewPrimaryLock(
        this.options.poolName,
        this.options.ownerId,
        this.options.primaryLockTtlSeconds,
      );
      const mayCommit =
        this.lifecycleState === PoolLifecycleState.RUNNING ||
        (this.lifecycleState === PoolLifecycleState.DRAINING && this.allowWarmupCommitWhileDraining);
      if (!stillPrimary || !mayCommit) {
        await this.killAndClose(sandbox);
        return;
      }
      await this.options.stateStore.putIdle(this.options.poolName, String(sandbox.id));
      committed = true;
      await sandbox.close().catch((error: unknown) => {
        this.options.logger?.warn?.("failed to close pooled sandbox client", {
          sandboxId: String(sandbox!.id),
          error,
        });
      });
    } catch (cause) {
      if (sandbox && !committed) await this.killAndClose(sandbox);
      throw cause;
    } finally {
      this.inFlightOperations -= 1;
    }
  }

  private async createSandbox(reason: PooledSandboxCreateReason, signal?: AbortSignal): Promise<Sandbox> {
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
        connectionConfig: this.options.connectionConfig,
        creationSpec: this.options.creationSpec,
        signal,
      });
    }
    return await Sandbox.create({
      ...this.options.creationSpec,
      connectionConfig: this.options.connectionConfig,
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
    const deadline = Date.now() + timeoutSeconds * 1000;
    let attempts = 0;
    let lastError: unknown;
    while (Date.now() < deadline) {
      signal?.throwIfAborted();
      attempts += 1;
      let healthy = false;
      try {
        healthy = healthCheck ? await healthCheck(sandbox) : await sandbox.isHealthy();
      } catch (error) {
        lastError = error;
        healthy = false;
      }
      if (healthy) return;
      await sleep(pollingIntervalMillis, signal);
    }
    throw new SandboxReadyTimeoutException({
      message: `Sandbox health check timed out after ${timeoutSeconds}s (${attempts} attempts).`,
      cause: lastError,
    });
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

  private ensureAcquireStillRunning(): void {
    if (this.lifecycleState === PoolLifecycleState.RUNNING) return;
    throw new PoolNotRunningException(this.options.poolName, this.lifecycleState);
  }

  private recordSuccess(): void {
    this.failureCount = 0;
    this.lastError = undefined;
    this.backoffUntilMs = 0;
    this.healthState = PoolHealthState.HEALTHY;
  }

  private recordFailure(error: unknown): void {
    this.failureCount += 1;
    this.lastError = error instanceof Error ? error.message : String(error);
    if (this.failureCount >= this.options.degradedThreshold) {
      this.healthState = PoolHealthState.DEGRADED;
      const backoffSeconds = Math.min(
        24 * 60 * 60,
        30 * 2 ** (this.failureCount - this.options.degradedThreshold),
      );
      this.backoffUntilMs = Date.now() + backoffSeconds * 1000;
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
      if (cause instanceof PoolStateStoreUnavailableException) throw cause;
      throw new PoolStateStoreUnavailableException(operation, cause);
    }
  }
}
