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
  DEFAULT_EXECD_PORT,
  DEFAULT_HEALTH_CHECK_POLLING_INTERVAL_MILLIS,
  DEFAULT_READY_TIMEOUT_SECONDS,
  SandboxReadyTimeoutException,
} from "@alibaba-group/opensandbox";
import { createExecdClient } from "@alibaba-group/opensandbox/internal";
import type { Sandbox } from "@alibaba-group/opensandbox";

import { createDefaultAdapterFactory } from "./factory/defaultAdapterFactory.js";
import type { AdapterFactory } from "./factory/adapterFactory.js";
import type { Codes } from "./services/codes.js";

export interface CodeInterpreterCreateOptions {
  adapterFactory?: AdapterFactory;
  /**
   * Skip the strict code-executor readiness check. The returned interpreter
   * may fail on first use if the execd daemon is not serving yet.
   */
  skipHealthCheck?: boolean;
  /**
   * Max time to wait for the code execution service (execd) health check, in seconds.
   */
  readyTimeoutSeconds?: number;
  /**
   * Polling interval for the code execution service health check, in milliseconds.
   */
  healthCheckPollingInterval?: number;
  /**
   * Optional signal used to cancel the health check.
   */
  signal?: AbortSignal;
}

/**
 * Strict health check script: verifies the code interpreter runtime (Jupyter
 * kernel gateway) is actually serving inside the sandbox. execd starts serving
 * /ping before the entrypoint launches Jupyter, and the setup stage may run
 * short-lived "jupyter kernelspec" helpers, so a daemon ping or a process-name
 * grep cannot prove the runtime is ready. Probing the Jupyter listen port
 * (127.0.0.1:${JUPYTER_PORT:-44771}, same default as the entrypoint) only
 * passes once the server accepts connections.
 */
const RUNTIME_CHECK_COMMAND =
  "bash -c 'exec 3<>/dev/tcp/127.0.0.1/${JUPYTER_PORT:-44771}' && exit 0 || exit 1";

function throwIfAborted(signal?: AbortSignal): void {
  signal?.throwIfAborted();
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  if (!signal) return new Promise((resolve) => setTimeout(resolve, ms));
  throwIfAborted(signal);

  return new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", onAbort);
      reject(signal.reason);
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

/**
 * Code interpreter facade (JS/TS).
 *
 * This class wraps an existing {@link Sandbox} and provides a high-level API for code execution.
 *
 * - Use {@link codes} to create contexts and run code.
 * - {@link files}, {@link commands}, and {@link metrics} are exposed for convenience and are
 *   the same instances as on the underlying {@link Sandbox}.
 */
export class CodeInterpreter {
  private constructor(
    readonly sandbox: Sandbox,
    readonly codes: Codes,
    private readonly execdBaseUrl: string,
    private readonly execdHeaders: Record<string, string> | undefined,
  ) {}

  static async create(sandbox: Sandbox, opts: CodeInterpreterCreateOptions = {}): Promise<CodeInterpreter> {
    const endpoint = await sandbox.getEndpoint(DEFAULT_EXECD_PORT);
    const execdBaseUrl = `${sandbox.connectionConfig.protocol}://${endpoint.endpoint}`;
    const execdHeaders = endpoint.headers;
    const adapterFactory = opts.adapterFactory ?? createDefaultAdapterFactory();
    const codes = adapterFactory.createCodes({
      sandbox,
      execdBaseUrl,
      endpointHeaders: endpoint.headers,
    });

    const interpreter = new CodeInterpreter(sandbox, codes, execdBaseUrl, execdHeaders);

    if (!(opts.skipHealthCheck ?? false)) {
      await interpreter.waitUntilReady({
        readyTimeoutSeconds: opts.readyTimeoutSeconds ?? DEFAULT_READY_TIMEOUT_SECONDS,
        pollingIntervalMillis:
          opts.healthCheckPollingInterval ?? DEFAULT_HEALTH_CHECK_POLLING_INTERVAL_MILLIS,
        signal: opts.signal,
      });
    }

    return interpreter;
  }

  get id() {
    return this.sandbox.id;
  }

  get files() {
    return this.sandbox.files;
  }

  get commands() {
    return this.sandbox.commands;
  }

  get metrics() {
    return this.sandbox.metrics;
  }

  /**
   * Ping the execd daemon on the interpreter's own endpoint.
   *
   * Prefers the codes service's optional `ping` capability; custom adapters
   * that do not implement it fall back to a direct execd probe built from the
   * sandbox connection config.
   */
  private async pingExecd(signal?: AbortSignal): Promise<boolean> {
    if (typeof this.codes.ping === "function") {
      return await this.codes.ping(signal);
    }
    const { error } = await this.execdPingClient().GET("/ping", {
      parseAs: "text",
      signal,
    });
    return error == null;
  }

  private execdPingClient() {
    return createExecdClient({
      baseUrl: this.execdBaseUrl,
      headers: this.execdHeaders ?? {},
      fetch: this.sandbox.connectionConfig.fetch,
    });
  }

  /**
   * Check if the code interpreter is healthy (strict check).
   *
   * Healthy means both:
   * - the code execution service (execd) answers `GET /ping`; and
   * - the code interpreter runtime (Jupyter kernel gateway) is serving
   *   inside the sandbox, verified by probing its listen port through the
   *   execd command API.
   *
   * Exceptions from either leg are treated as unhealthy.
   */
  async isHealthy(signal?: AbortSignal): Promise<boolean> {
    try {
      if (!(await this.pingExecd(signal))) {
        return false;
      }
      return await this.isRuntimeServing(signal);
    } catch {
      return false;
    }
  }

  private async isRuntimeServing(signal?: AbortSignal): Promise<boolean> {
    try {
      const execution = await this.sandbox.commands.run(
        RUNTIME_CHECK_COMMAND,
        undefined,
        undefined,
        signal,
      );
      return execution.error == null;
    } catch {
      return false;
    }
  }

  /**
   * Poll the strict health check (execd `GET /ping` + runtime process alive)
   * until it passes, or throw {@link SandboxReadyTimeoutException} when the
   * deadline expires.
   */
  async waitUntilReady(opts: {
    readyTimeoutSeconds: number;
    pollingIntervalMillis: number;
    signal?: AbortSignal;
  }): Promise<void> {
    const deadline = Date.now() + opts.readyTimeoutSeconds * 1000;
    let attempt = 0;
    let errorDetail = "Health check returned false continuously.";

    while (true) {
      throwIfAborted(opts.signal);
      if (Date.now() >= deadline) break;
      attempt++;
      try {
        const ok = await this.isHealthy(opts.signal);
        throwIfAborted(opts.signal);
        if (ok) {
          return;
        }
        errorDetail = "Health check returned false continuously.";
      } catch (err) {
        throwIfAborted(opts.signal);
        const message = err instanceof Error ? err.message : String(err);
        errorDetail = `Last health check error: ${message}`;
      }
      // Clamp the sleep to the remaining budget so the final failed check
      // does not overshoot the timeout by a full polling interval.
      const remaining = deadline - Date.now();
      if (remaining <= 0) break;
      await sleep(Math.min(opts.pollingIntervalMillis, remaining), opts.signal);
    }

    throw new SandboxReadyTimeoutException({
      message:
        `Code interpreter ${this.id} health check timed out after ` +
        `${opts.readyTimeoutSeconds}s (${attempt} attempts). ${errorDetail} ` +
        `The code execution service (execd) or the interpreter runtime (Jupyter) ` +
        `did not become ready. Pass skipHealthCheck to skip this check.`,
    });
  }
}
