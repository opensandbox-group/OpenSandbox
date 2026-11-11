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

import {
  context,
  propagation,
  SpanStatusCode,
  trace,
  type Attributes,
  type Span,
  type Tracer,
} from "@opentelemetry/api";
import { ConnectionConfig } from "../config/connection.js";

const INSTRUMENTATION_NAME = "com.alibaba.opensandbox.sandbox";

export const POOL_WARMUP_SPANS = {
  root: "pool.warmup",
  create: "pool.warmup.create",
  readiness: "pool.warmup.readiness",
  prepare: "pool.warmup.prepare",
  postPrepareReadiness: "pool.warmup.post_prepare_readiness",
  renew: "pool.warmup.renew",
  commit: "pool.warmup.commit",
} as const;

/** Best-effort, opt-in tracing for one pool's warmup pipeline. */
export class PoolTracer {
  private constructor(private readonly tracer?: Tracer) {}

  static from(connectionConfig: ConnectionConfig): PoolTracer {
    if (!connectionConfig.enableTracing) return new PoolTracer();
    try {
      return new PoolTracer(trace.getTracer(INSTRUMENTATION_NAME));
    } catch {
      return new PoolTracer();
    }
  }

  runWarmup<T>(attributes: Attributes, work: () => Promise<T>): Promise<T> {
    return this.runSpan(POOL_WARMUP_SPANS.root, attributes, work);
  }

  runPhase<T>(name: string, work: () => Promise<T>): Promise<T> {
    return this.runSpan(name, undefined, work);
  }

  /** Clone a config and inject the current W3C trace context into lifecycle headers. */
  createConnectionConfig(config: ConnectionConfig): ConnectionConfig {
    if (!this.tracer) return config;
    const headers = { ...config.headers };
    try {
      propagation.inject(context.active(), headers);
    } catch {
      return config;
    }
    return new ConnectionConfig({
      domain: config.domain,
      protocol: config.protocol,
      apiKey: config.apiKey,
      headers,
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

  private async runSpan<T>(
    name: string,
    attributes: Attributes | undefined,
    work: () => Promise<T>,
  ): Promise<T> {
    if (!this.tracer) return await work();
    let workPromise: Promise<T> | undefined;
    try {
      return await this.tracer.startActiveSpan(name, { attributes }, (span) => {
        workPromise = this.finishSpan(span, work);
        return workPromise;
      });
    } catch (error) {
      // Telemetry must never make a successful pool operation fail. A work
      // failure still passes through because finishSpan rethrows it.
      if (error instanceof PoolWorkFailure) throw error.cause;
      if (workPromise) {
        try {
          return await workPromise;
        } catch (workError) {
          if (workError instanceof PoolWorkFailure) throw workError.cause;
          throw workError;
        }
      }
      return await work();
    }
  }

  private async finishSpan<T>(span: Span, work: () => Promise<T>): Promise<T> {
    try {
      const result = await work();
      try {
        span.setAttribute("warmup.result", "success");
      } catch {
        // Best effort.
      }
      return result;
    } catch (cause) {
      try {
        span.setAttribute("warmup.result", "failure");
        span.setAttribute(
          "warmup.error.type",
          cause instanceof Error ? cause.name : typeof cause,
        );
        span.setStatus({ code: SpanStatusCode.ERROR });
        if (cause instanceof Error) span.recordException(cause);
      } catch {
        // Best effort.
      }
      throw new PoolWorkFailure(cause);
    } finally {
      try {
        span.end();
      } catch {
        // Best effort.
      }
    }
  }
}

class PoolWorkFailure {
  constructor(readonly cause: unknown) {}
}
