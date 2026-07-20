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

import { expect, test } from "vitest";

import { Sandbox } from "@alibaba-group/opensandbox";

import {
  TEST_API_KEY,
  TEST_DOMAIN,
  TEST_PROTOCOL,
  createConnectionConfig,
  getSandboxImage,
} from "./base_e2e.ts";

test("lifecycle metrics endpoint accepts SDK-shaped events", async () => {
  const url = `${TEST_PROTOCOL}://${TEST_DOMAIN}/v1/metrics/events`;
  const response = await fetch(url, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "OPEN-SANDBOX-API-KEY": TEST_API_KEY,
    },
    body: JSON.stringify({
      eventType: "sandbox.create",
      sandboxId: "e2e-metrics-direct-js",
      image: getSandboxImage(),
      createDurationMs: 42,
      success: true,
    }),
  });
  expect(response.status).toBe(204);
});

test(
  "Sandbox.create reports sandbox.create metrics to lifecycle server",
  async () => {
    const metricsPosts: Array<{ url: string; body: Record<string, unknown> }> =
      [];

    let connectionConfig = createConnectionConfig().withTransportIfMissing();
    const baseFetch = connectionConfig.fetch.bind(connectionConfig);
    Object.defineProperty(connectionConfig, "fetch", {
      configurable: true,
      get() {
        return async (input: RequestInfo | URL, init?: RequestInit) => {
          const url = String(input);
          if (url.includes("/metrics/events")) {
            const body = JSON.parse(String(init?.body ?? "{}"));
            metricsPosts.push({ url, body });
          }
          return baseFetch(input, init);
        };
      },
    });

    const sandbox = await Sandbox.create({
      connectionConfig,
      image: getSandboxImage(),
      timeoutSeconds: 5 * 60,
      readyTimeoutSeconds: 60,
      entrypoint: ["tail", "-f", "/dev/null"],
      env: { EXECD_API_GRACE_SHUTDOWN: "3s" },
      metadata: { tag: "e2e-lifecycle-metrics" },
      healthCheckPollingInterval: 200,
    });

    try {
      const deadline = Date.now() + 5_000;
      while (Date.now() < deadline && metricsPosts.length < 1) {
        await new Promise((r) => setTimeout(r, 50));
      }
      expect(metricsPosts.length).toBeGreaterThanOrEqual(1);
      const event = metricsPosts[0].body;
      expect(event.eventType).toBe("sandbox.create");
      expect(event.success).toBe(true);
      expect(event.sandboxId).toBe(sandbox.id);
      expect(event.sdkLanguage).toBeUndefined();
      expect(event.sdkVersion).toBeUndefined();
      expect(event.createDurationMs).toBeGreaterThan(0);
      console.log(
        `sandbox.create metrics createDurationMs=${event.createDurationMs}ms sandboxId=${event.sandboxId}`
      );
      expect(event.image).toBeTruthy();
      expect(metricsPosts[0].url).toMatch(/\/v1\/metrics\/events$/);
    } finally {
      try {
        await sandbox.kill();
      } catch {
        // best-effort cleanup
      }
    }
  },
  5 * 60_000
);
