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

import {
  DEFAULT_ENTRYPOINT,
  DEFAULT_EGRESS_PORT,
  DEFAULT_EXECD_PORT,
  DEFAULT_HEALTH_CHECK_POLLING_INTERVAL_MILLIS,
  DEFAULT_READY_TIMEOUT_SECONDS,
  DEFAULT_RESOURCE_LIMITS,
  DEFAULT_TIMEOUT_SECONDS,
} from "./core/constants.js";
import { ConnectionConfig, type ConnectionConfigOptions } from "./config/connection.js";
import type { SandboxFiles } from "./services/filesystem.js";
import type { Egress } from "./services/egress.js";
import { createDefaultAdapterFactory } from "./factory/defaultAdapterFactory.js";
import type { AdapterFactory } from "./factory/adapterFactory.js";

import type { Sandboxes } from "./services/sandboxes.js";
import type { ExecdCommands } from "./services/execdCommands.js";
import type { ExecdHealth } from "./services/execdHealth.js";
import type { ExecdMetrics } from "./services/execdMetrics.js";
import type {
  CreateSandboxRequest,
  Endpoint,
  NetworkPolicy,
  NetworkRule,
  PlatformSpec,
  RenewSandboxExpirationResponse,
  SandboxId,
  SandboxInfo,
  SandboxMetadataPatch,
  Volume,
} from "./models/sandboxes.js";
import { SandboxReadyTimeoutException } from "./core/exceptions.js";

const HOST_PATH_PATTERN = /^([/]|[A-Za-z]:[\\/])/;

export interface SandboxCreateOptions {
  /**
   * Connection configuration for calling the OpenSandbox Lifecycle API and the sandbox's execd API.
   */
  connectionConfig?: ConnectionConfig | ConnectionConfigOptions;
  /**
   * Advanced override: inject a custom adapter factory (custom transports, dependency injection).
   */
  adapterFactory?: AdapterFactory;

  /**
   * Container image uri, e.g. `python:3.11`
   */
  image?:
    | string
    | { uri: string; auth?: { username: string; password: string } };
  /**
   * Snapshot identifier to restore from.
   * Mutually exclusive with `image`.
   */
  snapshotId?: string;

  /**
   * Entrypoint command for the sandbox (defaults to tail -f /dev/null).
   */
  entrypoint?: string[];
  /**
   * Environment variables to inject into the sandbox runtime.
   */
  env?: Record<string, string>;
  /**
   * Custom metadata tags (used for filtering/management).
   */
  metadata?: Record<string, string>;
  /**
   * Optional outbound network policy for the sandbox.
   * If provided without defaultAction, defaults to "deny".
   */
  networkPolicy?: NetworkPolicy;
  /**
   * Optional list of volume mounts for persistent storage.
   * Each volume specifies a backend (host path, PVC, or OSSFS) and mount configuration.
   */
  volumes?: Volume[];
  /**
   * Opaque extension parameters passed through to the server as-is.
   */
  extensions?: Record<string, string>;
  /**
   * Optional runtime platform constraint used for provisioning.
   */
  platform?: PlatformSpec;
  /**
   * Whether to enable secured access for sandbox endpoints.
   */
  secureAccess?: boolean;

  /**
   * Resource limits applied to the sandbox container.
   *
   * This is forwarded to the Lifecycle API as `resourceLimits`.
   */
  resource?: Record<string, string>;
  /**
   * Sandbox timeout in seconds. Set to `null` to require explicit cleanup.
   */
  timeoutSeconds?: number | null;

  /**
   * Skip readiness checks during create/connect.
   *
   * When true, the SDK will not wait for lifecycle state `Running` or perform the health check.
   * The returned sandbox instance may not be ready yet.
   */
  skipHealthCheck?: boolean;
  /**
   * Optional custom readiness check used by {@link Sandbox.waitUntilReady}.
   *
   * If provided, the SDK will call this function during readiness checks instead of
   * using the default `execd` ping check.
   */
  healthCheck?: (sbx: Sandbox) => boolean | Promise<boolean>;
  readyTimeoutSeconds?: number;
  healthCheckPollingInterval?: number;
}

export interface SandboxConnectOptions {
  /**
   * Connection configuration for calling the OpenSandbox APIs.
   */
  connectionConfig?: ConnectionConfig | ConnectionConfigOptions;
  /**
   * Advanced override: inject a custom adapter factory (custom transports, dependency injection).
   */
  adapterFactory?: AdapterFactory;
  /**
   * ID of the existing sandbox to connect to.
   */
  sandboxId: SandboxId;

  /**
   * Skip readiness checks after connecting.
   */
  skipHealthCheck?: boolean;
  /**
   * Optional custom readiness check used by {@link Sandbox.waitUntilReady}.
   */
  healthCheck?: (sbx: Sandbox) => boolean | Promise<boolean>;
  /**
   * Max time to wait for readiness.
   */
  readyTimeoutSeconds?: number;
  /**
   * Polling interval for readiness checks (milliseconds).
   */
  healthCheckPollingInterval?: number;
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}

function toImageSpec(
  image: NonNullable<SandboxCreateOptions["image"]>
): NonNullable<CreateSandboxRequest["image"]> {
  if (typeof image === "string") return { uri: image };
  return { uri: image.uri, auth: image.auth };
}

export class Sandbox {
  readonly id: SandboxId;
  readonly connectionConfig: ConnectionConfig;

  /**
   * Lifecycle (sandbox management) service.
   */
  readonly sandboxes: Sandboxes;

  /**
   * Execd services.
   */
  readonly commands: ExecdCommands;
  /**
   * High-level filesystem facade (JS-friendly).
   */
  readonly files: SandboxFiles;
  readonly health: ExecdHealth;
  readonly metrics: ExecdMetrics;

  /**
   * Internal state kept out of the public instance shape.
   *
   * This avoids nominal typing issues when multiple copies of the SDK exist in a dependency graph.
   */
  private static readonly _priv = new WeakMap<
    Sandbox,
    {
      adapterFactory: AdapterFactory;
      lifecycleBaseUrl: string;
      execdBaseUrl: string;
      egress: Egress;
    }
  >();

  private constructor(opts: {
    id: SandboxId;
    connectionConfig: ConnectionConfig;
    adapterFactory: AdapterFactory;
    lifecycleBaseUrl: string;
    execdBaseUrl: string;
    sandboxes: Sandboxes;
    commands: ExecdCommands;
    files: SandboxFiles;
    health: ExecdHealth;
    metrics: ExecdMetrics;
    egress: Egress;
  }) {
    this.id = opts.id;
    this.connectionConfig = opts.connectionConfig;
    Sandbox._priv.set(this, {
      adapterFactory: opts.adapterFactory,
      lifecycleBaseUrl: opts.lifecycleBaseUrl,
      execdBaseUrl: opts.execdBaseUrl,
      egress: opts.egress,
    });

    this.sandboxes = opts.sandboxes;
    this.commands = opts.commands;
    this.files = opts.files;
    this.health = opts.health;
    this.metrics = opts.metrics;
  }

  static async create(opts: SandboxCreateOptions): Promise<Sandbox> {
    if ((opts.image == null) === (opts.snapshotId == null)) {
      throw new Error("Exactly one of image or snapshotId must be provided");
    }

    // Validate volumes before allocating transport resources.
    if (opts.volumes) {
      for (const vol of opts.volumes) {
        const backendsSpecified = [vol.host, vol.pvc, vol.ossfs].filter((b) => b != null).length;
        if (backendsSpecified === 0) {
          throw new Error(
            `Volume '${vol.name}' must specify exactly one backend (host, pvc, ossfs), but none was provided.`
          );
        }
        if (backendsSpecified > 1) {
          throw new Error(
            `Volume '${vol.name}' must specify exactly one backend (host, pvc, ossfs), but multiple were provided.`
          );
        }
        if (vol.host && !HOST_PATH_PATTERN.test(vol.host.path)) {
          throw new Error(
            "Host path must be an absolute path starting with '/' or a Windows drive letter (e.g. 'C:\\' or 'D:/')"
          );
        }
      }
    }

    const baseConnectionConfig =
      opts.connectionConfig instanceof ConnectionConfig
        ? opts.connectionConfig
        : new ConnectionConfig(opts.connectionConfig);
    const connectionConfig = baseConnectionConfig.withTransportIfMissing();
    const lifecycleBaseUrl = connectionConfig.getBaseUrl();
    const adapterFactory = opts.adapterFactory ?? createDefaultAdapterFactory();

    let sandboxes: Sandboxes;
    try {
      sandboxes = adapterFactory.createLifecycleStack({
        connectionConfig,
        lifecycleBaseUrl,
      }).sandboxes;
    } catch (err) {
      await connectionConfig.closeTransport();
      throw err;
    }

    const rawTimeout = opts.timeoutSeconds ?? DEFAULT_TIMEOUT_SECONDS;
    const timeoutSeconds =
      opts.timeoutSeconds === null
        ? null
        : Math.floor(rawTimeout);
    if (timeoutSeconds !== null && !Number.isFinite(timeoutSeconds)) {
      throw new Error(
        `timeoutSeconds must be a finite number, got ${opts.timeoutSeconds}`
      );
    }

    const req: CreateSandboxRequest = {
      image: opts.image == null ? undefined : toImageSpec(opts.image),
      snapshotId: opts.snapshotId,
      entrypoint: opts.entrypoint ?? DEFAULT_ENTRYPOINT,
      resourceLimits: opts.resource ?? DEFAULT_RESOURCE_LIMITS,
      secureAccess: opts.secureAccess ?? false,
      env: opts.env ?? {},
      metadata: opts.metadata ?? {},
      networkPolicy: opts.networkPolicy
        ? {
            ...opts.networkPolicy,
            defaultAction: opts.networkPolicy.defaultAction ?? "deny",
          }
        : undefined,
      volumes: opts.volumes,
      extensions: opts.extensions ?? {},
      platform: opts.platform,
    };
    if (timeoutSeconds !== null) {
      req.timeout = timeoutSeconds;
    }

    let sandboxId: SandboxId | undefined;
    try {
      const created = await sandboxes.createSandbox(req);
      sandboxId = created.id as SandboxId;

      const endpoint = await sandboxes.getSandboxEndpoint(
        sandboxId,
        DEFAULT_EXECD_PORT,
        connectionConfig.useServerProxy
      );
      const egressEndpoint = await sandboxes.getSandboxEndpoint(
        sandboxId,
        DEFAULT_EGRESS_PORT,
        connectionConfig.useServerProxy
      );
      const execdBaseUrl = `${connectionConfig.protocol}://${endpoint.endpoint}`;
      const egressBaseUrl = `${connectionConfig.protocol}://${egressEndpoint.endpoint}`;

      const { commands, files, health, metrics } =
        adapterFactory.createExecdStack({
          connectionConfig,
          execdBaseUrl,
          endpointHeaders: endpoint.headers,
        });
      const { egress } = adapterFactory.createEgressStack({
        connectionConfig,
        egressBaseUrl,
        endpointHeaders: egressEndpoint.headers,
      });

      const sbx = new Sandbox({
        id: sandboxId,
        connectionConfig,
        adapterFactory,
        lifecycleBaseUrl,
        execdBaseUrl,
        sandboxes,
        commands,
        files,
        health,
        metrics,
        egress,
      });

      if (!(opts.skipHealthCheck ?? false)) {
        await sbx.waitUntilReady({
          readyTimeoutSeconds:
            opts.readyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
          pollingIntervalMillis:
            opts.healthCheckPollingInterval ??
            DEFAULT_HEALTH_CHECK_POLLING_INTERVAL_MILLIS,
          healthCheck: opts.healthCheck,
        });
      }

      return sbx;
    } catch (err) {
      if (sandboxId) {
        try {
          await sandboxes.deleteSandbox(sandboxId);
        } catch {
          // Ignore cleanup failure; surface original error.
        }
      }
      await connectionConfig.closeTransport();
      throw err;
    }
  }

  static async connect(opts: SandboxConnectOptions): Promise<Sandbox> {
    const baseConnectionConfig =
      opts.connectionConfig instanceof ConnectionConfig
        ? opts.connectionConfig
        : new ConnectionConfig(opts.connectionConfig);
    const connectionConfig = baseConnectionConfig.withTransportIfMissing();
    const adapterFactory = opts.adapterFactory ?? createDefaultAdapterFactory();
    const lifecycleBaseUrl = connectionConfig.getBaseUrl();

    let sandboxes: Sandboxes;
    try {
      sandboxes = adapterFactory.createLifecycleStack({
        connectionConfig,
        lifecycleBaseUrl,
      }).sandboxes;
    } catch (err) {
      await connectionConfig.closeTransport();
      throw err;
    }

    try {
      const endpoint = await sandboxes.getSandboxEndpoint(
        opts.sandboxId,
        DEFAULT_EXECD_PORT,
        connectionConfig.useServerProxy
      );
      const egressEndpoint = await sandboxes.getSandboxEndpoint(
        opts.sandboxId,
        DEFAULT_EGRESS_PORT,
        connectionConfig.useServerProxy
      );
      const execdBaseUrl = `${connectionConfig.protocol}://${endpoint.endpoint}`;
      const egressBaseUrl = `${connectionConfig.protocol}://${egressEndpoint.endpoint}`;
      const { commands, files, health, metrics } =
        adapterFactory.createExecdStack({
          connectionConfig,
          execdBaseUrl,
          endpointHeaders: endpoint.headers,
        });
      const { egress } = adapterFactory.createEgressStack({
        connectionConfig,
        egressBaseUrl,
        endpointHeaders: egressEndpoint.headers,
      });

      const sbx = new Sandbox({
        id: opts.sandboxId,
        connectionConfig,
        adapterFactory,
        lifecycleBaseUrl,
        execdBaseUrl,
        sandboxes,
        commands,
        files,
        health,
        metrics,
        egress,
      });

      if (!(opts.skipHealthCheck ?? false)) {
        await sbx.waitUntilReady({
          readyTimeoutSeconds:
            opts.readyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
          pollingIntervalMillis:
            opts.healthCheckPollingInterval ??
            DEFAULT_HEALTH_CHECK_POLLING_INTERVAL_MILLIS,
          healthCheck: opts.healthCheck,
        });
      }

      return sbx;
    } catch (err) {
      await connectionConfig.closeTransport();
      throw err;
    }
  }

  async getInfo(): Promise<SandboxInfo> {
    return await this.sandboxes.getSandbox(this.id);
  }

  async isHealthy(): Promise<boolean> {
    try {
      return await this.health.ping();
    } catch {
      return false;
    }
  }

  async getMetrics() {
    return await this.metrics.getMetrics();
  }

  async pause(): Promise<void> {
    await this.sandboxes.pauseSandbox(this.id);
  }

  /**
   * Resume a paused sandbox and return a fresh, connected Sandbox instance.
   *
   * After resume, the execd endpoint may change, so this method returns a new
   * {@link Sandbox} instance with a refreshed execd base URL.
   */
  async resume(
    opts: {
      skipHealthCheck?: boolean;
      readyTimeoutSeconds?: number;
      healthCheckPollingInterval?: number;
    } = {}
  ): Promise<Sandbox> {
    await this.sandboxes.resumeSandbox(this.id);
    return await Sandbox.connect({
      sandboxId: this.id,
      connectionConfig: this.connectionConfig,
      adapterFactory: Sandbox._priv.get(this)!.adapterFactory,
      skipHealthCheck: opts.skipHealthCheck ?? false,
      readyTimeoutSeconds: opts.readyTimeoutSeconds,
      healthCheckPollingInterval: opts.healthCheckPollingInterval,
    });
  }

  /**
   * Resume a paused sandbox by id, then connect to its execd endpoint.
   */
  static async resume(opts: SandboxConnectOptions): Promise<Sandbox> {
    const baseConnectionConfig =
      opts.connectionConfig instanceof ConnectionConfig
        ? opts.connectionConfig
        : new ConnectionConfig(opts.connectionConfig);
    const adapterFactory = opts.adapterFactory ?? createDefaultAdapterFactory();
    const resumeConnectionConfig = baseConnectionConfig.withTransportIfMissing();
    const lifecycleBaseUrl = resumeConnectionConfig.getBaseUrl();

    let sandboxes: Sandboxes;
    try {
      sandboxes = adapterFactory.createLifecycleStack({
        connectionConfig: resumeConnectionConfig,
        lifecycleBaseUrl,
      }).sandboxes;
      await sandboxes.resumeSandbox(opts.sandboxId);
    } catch (err) {
      await resumeConnectionConfig.closeTransport();
      throw err;
    }

    await resumeConnectionConfig.closeTransport();
    return await Sandbox.connect({ ...opts, connectionConfig: baseConnectionConfig, adapterFactory });
  }

  async kill(): Promise<void> {
    await this.sandboxes.deleteSandbox(this.id);
  }

  /**
   * Release any client-side resources (e.g. Node.js HTTP agents) owned by this Sandbox instance.
   */
  async close(): Promise<void> {
    await this.connectionConfig.closeTransport();
  }

  /**
   * Renew expiration by setting expiresAt to now + timeoutSeconds.
   */
  async renew(timeoutSeconds: number): Promise<RenewSandboxExpirationResponse> {
    const expiresAt = new Date(
      Date.now() + timeoutSeconds * 1000
    ).toISOString();
    return await this.sandboxes.renewSandboxExpiration(this.id, { expiresAt });
  }

  async patchMetadata(patch: SandboxMetadataPatch): Promise<SandboxInfo> {
    return await this.sandboxes.patchSandboxMetadata(this.id, patch);
  }

  async getEgressPolicy(): Promise<NetworkPolicy> {
    return await Sandbox._priv.get(this)!.egress.getPolicy();
  }

  async patchEgressRules(rules: NetworkRule[]): Promise<void> {
    await Sandbox._priv.get(this)!.egress.patchRules(rules);
  }

  /**
   * Get sandbox endpoint for a port (STRICT: no scheme), e.g. "localhost:44772" or "domain/route/.../44772".
   */
  async getEndpoint(port: number): Promise<Endpoint> {
    return await this.sandboxes.getSandboxEndpoint(
      this.id,
      port,
      this.connectionConfig.useServerProxy
    );
  }

  /**
   * Get signed endpoint URL with an OSEP-0011 route token that expires at the given Unix epoch timestamp (seconds).
   */
  async getSignedEndpoint(port: number, expires: number): Promise<Endpoint> {
    return await this.sandboxes.getSignedEndpoint(this.id, port, expires);
  }

  /**
   * Get absolute endpoint URL with scheme (convenience for HTTP clients).
   */
  async getEndpointUrl(port: number): Promise<string> {
    const ep = await this.getEndpoint(port);
    return `${this.connectionConfig.protocol}://${ep.endpoint}`;
  }

  async waitUntilReady(opts: {
    readyTimeoutSeconds: number;
    pollingIntervalMillis: number;
    healthCheck?: (sbx: Sandbox) => boolean | Promise<boolean>;
  }): Promise<void> {
    const deadline = Date.now() + opts.readyTimeoutSeconds * 1000;
    let attempt = 0;
    let errorDetail = "Health check returned false continuously.";

    const buildTimeoutMessage = () => {
      const context = `domain=${this.connectionConfig.domain}, useServerProxy=${this.connectionConfig.useServerProxy}`;
      let suggestion =
        "If this sandbox runs in Docker bridge or remote-network mode, consider enabling useServerProxy=true.";
      if (!this.connectionConfig.useServerProxy) {
        suggestion += " You can also configure server-side [docker].host_ip for direct endpoint access.";
      }
      return `Sandbox health check timed out after ${opts.readyTimeoutSeconds}s (${attempt} attempts). ${errorDetail} Connection context: ${context}. ${suggestion}`;
    };

    // Wait until execd becomes reachable and passes health check.
    while (true) {
      if (Date.now() > deadline) {
        throw new SandboxReadyTimeoutException({
          message: buildTimeoutMessage(),
        });
      }
      attempt++;
      try {
        if (opts.healthCheck) {
          const ok = await opts.healthCheck(this);
          if (ok) {
            return;
          }
        } else {
          const ok = await this.health.ping();
          if (ok) {
            return;
          }
        }
        errorDetail = "Health check returned false continuously.";
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        errorDetail = `Last health check error: ${message}`;
      }
      await sleep(opts.pollingIntervalMillis);
    }
  }
}
