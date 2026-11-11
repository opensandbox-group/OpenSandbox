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

import type { ConnectionConfig, ConnectionConfigOptions } from "./config/connection.js";
import type { Sandbox, SandboxCreateOptions } from "./sandbox.js";

export enum AcquirePolicy {
  DIRECT_CREATE = "DIRECT_CREATE",
  FAIL_FAST = "FAIL_FAST",
  RETRY_NEXT_IDLE = "RETRY_NEXT_IDLE",
  RETRY_NEXT_IDLE_THEN_CREATE = "RETRY_NEXT_IDLE_THEN_CREATE",
}

export enum PoolLifecycleState {
  NOT_STARTED = "NOT_STARTED",
  STARTING = "STARTING",
  RUNNING = "RUNNING",
  DRAINING = "DRAINING",
  STOPPED = "STOPPED",
}

export enum PoolState {
  HEALTHY = "HEALTHY",
  DEGRADED = "DEGRADED",
  DRAINING = "DRAINING",
  STOPPED = "STOPPED",
}

export enum PoolDestroyState {
  ACTIVE = "ACTIVE",
  DESTROYING = "DESTROYING",
  DESTROYED = "DESTROYED",
}

export enum PoolDestroyStrategy {
  FORCE = "FORCE",
}

export interface IdleEntry {
  sandboxId: string;
  expiresAt: Date;
}

export interface TakeIdleResult {
  sandboxId?: string;
  discardedAliveSandboxIds: string[];
}

export interface ReapResult {
  discardedAliveSandboxIds: string[];
}

export interface StoreCounters {
  idleCount: number;
}

/**
 * Persistence contract used by a client-side sandbox pool.
 *
 * Distributed implementations must make each method atomic. In particular,
 * taking an idle entry must remove it in the same operation, `putIdle` must be
 * idempotent for a `(poolName, sandboxId)` pair, and primary-lock renew/release
 * must only succeed for the current owner.
 */
export interface PoolStateStore {
  tryTakeIdle(poolName: string): Promise<string | undefined>;
  tryTakeIdleWithMinTtl(poolName: string, minRemainingTtlSeconds: number): Promise<TakeIdleResult>;
  putIdle(poolName: string, sandboxId: string): Promise<void>;
  removeIdle(poolName: string, sandboxId: string): Promise<void>;
  tryAcquirePrimaryLock(poolName: string, ownerId: string, ttlSeconds: number): Promise<boolean>;
  renewPrimaryLock(poolName: string, ownerId: string, ttlSeconds: number): Promise<boolean>;
  releasePrimaryLock(poolName: string, ownerId: string): Promise<void>;
  reapExpiredIdle(poolName: string, now: Date): Promise<void>;
  reapExpiredIdleWithMinTtl(
    poolName: string,
    now: Date,
    minRemainingTtlSeconds: number,
  ): Promise<ReapResult>;
  snapshotCounters(poolName: string): Promise<StoreCounters>;
  snapshotIdleEntries(poolName: string): Promise<IdleEntry[]>;
  getMaxIdle(poolName: string): Promise<number | undefined>;
  setMaxIdle(poolName: string, maxIdle: number): Promise<void>;
  setIdleEntryTtl(poolName: string, ttlSeconds: number): Promise<void>;
  getDestroyState(poolName: string): Promise<PoolDestroyState>;
  beginDestroy(poolName: string, ownerId: string): Promise<void>;
  clearPoolState(poolName: string): Promise<void>;
  markDestroyed(poolName: string, ownerId: string, tombstoneTtlSeconds?: number | null): Promise<void>;
}

export interface PoolSnapshot {
  state: PoolState;
  lifecycleState: PoolLifecycleState;
  idleCount: number;
  maxIdle: number;
  failureCount: number;
  backoffActive: boolean;
  lastError?: string;
  inFlightOperations: number;
}

export type PoolCreationSpec = Omit<
  SandboxCreateOptions,
  | "connectionConfig"
  | "timeoutSeconds"
  | "skipHealthCheck"
  | "readyTimeoutSeconds"
  | "healthCheckPollingInterval"
  | "healthCheck"
  | "signal"
>;

export enum PooledSandboxCreateReason {
  WARMUP = "WARMUP",
  DIRECT_CREATE = "DIRECT_CREATE",
}

export interface PooledSandboxCreateContext {
  poolName: string;
  ownerId: string;
  idleTimeoutSeconds: number;
  reason: PooledSandboxCreateReason;
  readyTimeoutSeconds: number;
  healthCheckPollingIntervalMillis: number;
  skipHealthCheck: boolean;
  healthCheck?: PoolHealthCheck;
  connectionConfig: ConnectionConfig;
  createConnectionConfig: ConnectionConfig;
  creationSpec: PoolCreationSpec;
  signal?: AbortSignal;
}

export type PooledSandboxCreator = (context: PooledSandboxCreateContext) => Promise<Sandbox>;
export type PoolHealthCheck = (sandbox: Sandbox) => boolean | Promise<boolean>;
export type PoolSandboxPreparer = (sandbox: Sandbox) => void | Promise<void>;

export interface PoolLogger {
  debug?(message: string, fields?: Record<string, unknown>): void;
  info?(message: string, fields?: Record<string, unknown>): void;
  warn?(message: string, fields?: Record<string, unknown>): void;
}

export interface SandboxPoolOptions {
  poolName: string;
  maxIdle: number;
  connectionConfig: ConnectionConfig | ConnectionConfigOptions;
  creationSpec?: PoolCreationSpec;
  sandboxCreator?: PooledSandboxCreator;
  stateStore?: PoolStateStore;
  ownerId?: string;
  warmupCreateQps?: number;
  warmupConcurrency?: number;
  primaryLockTtlSeconds?: number;
  degradedThreshold?: number;
  idleTimeoutSeconds?: number;
  drainTimeoutSeconds?: number;
  acquireMinRemainingTtlSeconds?: number;
  acquireReadyTimeoutSeconds?: number;
  acquireHealthCheckPollingIntervalMillis?: number;
  acquireHealthCheck?: PoolHealthCheck;
  acquireSkipHealthCheck?: boolean;
  maxAcquireRetries?: number;
  warmupReadyTimeoutSeconds?: number;
  warmupHealthCheckInitialDelayMillis?: number;
  warmupHealthCheckPollingIntervalMillis?: number;
  warmupHealthCheck?: PoolHealthCheck;
  warmupSandboxPreparer?: PoolSandboxPreparer;
  warmupPostPrepareHealthCheck?: PoolHealthCheck;
  warmupPostPrepareHealthCheckTimeoutSeconds?: number;
  warmupSkipHealthCheck?: boolean;
  logger?: PoolLogger;
}

export interface SandboxAcquireOptions {
  sandboxTimeoutSeconds?: number;
  policy?: AcquirePolicy;
  minRemainingTtlSeconds?: number;
  skipHealthCheck?: boolean;
  signal?: AbortSignal;
}

export interface PoolDestroyOptions {
  strategy?: PoolDestroyStrategy;
  drainTimeoutSeconds?: number;
  tombstoneTtlSeconds?: number | null;
}

export interface PoolDestroyResult {
  poolName: string;
  state: PoolDestroyState;
  drainedIdleCount: number;
  killedIdleCount: number;
  persistentStateCleared: boolean;
}
