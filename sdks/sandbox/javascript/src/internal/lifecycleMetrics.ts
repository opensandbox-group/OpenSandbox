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

import { DEFAULT_USER_AGENT } from "../core/constants.js";
import type { ConnectionConfig } from "../config/connection.js";

export type SandboxCreateMetricsPayload = {
  eventType: "sandbox.create";
  sandboxId?: string;
  image?: string;
  createDurationMs: number;
  sdkLanguage: "typescript";
  sdkVersion: string;
  success: boolean;
};

function sdkVersionFromUserAgent(userAgent: string | undefined): string {
  const match = (userAgent ?? DEFAULT_USER_AGENT).match(
    /OpenSandbox-JS-SDK\/([^\s]+)/
  );
  return match?.[1] ?? "0.0.0";
}

/**
 * Best-effort fire-and-forget report of sandbox create latency.
 * Never throws and never awaits for callers.
 */
export function reportSandboxCreateMetric(
  connectionConfig: ConnectionConfig,
  opts: {
    sandboxId?: string;
    image?: string;
    createDurationMs: number;
    success: boolean;
  }
): void {
  const payload: SandboxCreateMetricsPayload = {
    eventType: "sandbox.create",
    createDurationMs: Math.max(0, Math.floor(opts.createDurationMs)),
    sdkLanguage: "typescript",
    sdkVersion: sdkVersionFromUserAgent(connectionConfig.userAgent),
    success: opts.success,
  };
  if (opts.sandboxId) {
    payload.sandboxId = opts.sandboxId;
  }
  if (opts.image) {
    payload.image = opts.image;
  }

  const url = `${connectionConfig.getBaseUrl().replace(/\/$/, "")}/metrics/events`;
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(connectionConfig.headers ?? {}),
  };
  if (
    connectionConfig.apiKey &&
    !headers["OPEN-SANDBOX-API-KEY"]
  ) {
    headers["OPEN-SANDBOX-API-KEY"] = connectionConfig.apiKey;
  }

  void connectionConfig
    .fetch(url, {
      method: "POST",
      headers,
      body: JSON.stringify(payload),
    })
    .then(async (res) => {
      // Drain body to allow connection reuse; ignore status.
      try {
        await res.arrayBuffer();
      } catch {
        // ignore
      }
    })
    .catch(() => {
      // Metrics must never affect create UX.
    });
}
