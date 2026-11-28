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

import type { ExecdClient } from "../openapi/execdClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { ExecdHealth } from "../services/execdHealth.js";

/**
 * Keep stalled health connections from consuming the whole readiness budget.
 * Mirrors the Python SDK's `HEALTH_PING_TIMEOUT` (d8640951f): a ping whose
 * connection is accepted but never answered fails after this cap, and the
 * readiness loop retries while budget remains, instead of holding the whole
 * `readyTimeoutSeconds` budget on one stalled probe.
 */
export const HEALTH_PING_TIMEOUT_SECONDS = 5;

export class HealthAdapter implements ExecdHealth {
  constructor(
    private readonly client: ExecdClient,
    private readonly requestTimeoutSeconds: number | undefined,
  ) {}

  async ping(signal?: AbortSignal): Promise<boolean> {
    const perPingSeconds =
      this.requestTimeoutSeconds === undefined
        ? HEALTH_PING_TIMEOUT_SECONDS
        : Math.min(this.requestTimeoutSeconds, HEALTH_PING_TIMEOUT_SECONDS);
    const controller = new AbortController();
    const onAbort = () => controller.abort(signal!.reason);
    if (signal) {
      if (signal.aborted) {
        controller.abort(signal.reason);
      } else {
        signal.addEventListener("abort", onAbort, { once: true });
      }
    }
    const timer = setTimeout(
      () => controller.abort(new Error(`Execd ping timed out after ${perPingSeconds}s`)),
      perPingSeconds * 1000,
    );
    try {
      const { error, response } = await this.client.GET("/ping", {
        parseAs: "text",
        signal: controller.signal,
      });
      throwOnOpenApiFetchError({ error, response }, "Execd ping failed");
      return true;
    } finally {
      clearTimeout(timer);
      if (signal) signal.removeEventListener("abort", onAbort);
    }
  }
}
