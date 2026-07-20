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
      durationMs: 42,
      success: true,
    }),
  });
  expect(response.status).toBe(204);
});

test("lifecycle metrics endpoint accepts durationMs-based events", async () => {
  const url = `${TEST_PROTOCOL}://${TEST_DOMAIN}/v1/metrics/events`;
  for (const eventType of ["sandbox.resume", "sandbox.pause", "sandbox.kill"]) {
    const response = await fetch(url, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "OPEN-SANDBOX-API-KEY": TEST_API_KEY,
      },
      body: JSON.stringify({
        eventType,
        sandboxId: "e2e-metrics-direct-js",
        durationMs: 42,
        success: true,
      }),
    });
    expect(response.status, `eventType=${eventType}`).toBe(204);
  }
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
      expect(event.durationMs).toBeGreaterThan(0);
      console.log(
        `sandbox.create metrics durationMs=${event.durationMs}ms sandboxId=${event.sandboxId}`
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

test(
  "pause/resume/kill report lifecycle metrics to lifecycle server",
  async () => {
    const metricsPosts: Array<Record<string, unknown>> = [];

    // Same intercepted config is reused by pause/resume/kill, so every
    // fire-and-forget metrics POST goes through this fetch wrapper.
    const connectionConfig = createConnectionConfig().withTransportIfMissing();
    const baseFetch = connectionConfig.fetch.bind(connectionConfig);
    Object.defineProperty(connectionConfig, "fetch", {
      configurable: true,
      get() {
        return async (input: RequestInfo | URL, init?: RequestInit) => {
          const url = String(input);
          if (url.includes("/metrics/events")) {
            metricsPosts.push(JSON.parse(String(init?.body ?? "{}")));
          }
          return baseFetch(input, init);
        };
      },
    });

    const eventsOf = (eventType: string) =>
      metricsPosts.filter((e) => e.eventType === eventType);
    const waitFor = async (eventType: string) => {
      const deadline = Date.now() + 5_000;
      while (Date.now() < deadline && eventsOf(eventType).length < 1) {
        await new Promise((r) => setTimeout(r, 50));
      }
      expect(eventsOf(eventType).length, `eventType=${eventType}`).toBeGreaterThanOrEqual(1);
    };

    const sandbox = await Sandbox.create({
      connectionConfig,
      image: getSandboxImage(),
      timeoutSeconds: 5 * 60,
      readyTimeoutSeconds: 60,
      entrypoint: ["tail", "-f", "/dev/null"],
      env: { EXECD_API_GRACE_SHUTDOWN: "3s" },
      metadata: { tag: "e2e-lifecycle-metrics-ops" },
      healthCheckPollingInterval: 200,
    });

    let killed = false;
    let resumed: Sandbox | undefined;
    try {
      await sandbox.pause();
      await waitFor("sandbox.pause");

      resumed = await sandbox.resume();
      await waitFor("sandbox.resume");

      await resumed.kill();
      killed = true;
      await waitFor("sandbox.kill");
    } finally {
      if (!killed) {
        try {
          await (resumed ?? sandbox).kill();
        } catch {
          // best-effort cleanup
        }
      }
    }

    for (const eventType of ["sandbox.pause", "sandbox.resume", "sandbox.kill"]) {
      const event = eventsOf(eventType)[0];
      expect(event.success, `eventType=${eventType}`).toBe(true);
      expect(event.sandboxId, `eventType=${eventType}`).toBe(sandbox.id);
      expect(typeof event.durationMs, `eventType=${eventType}`).toBe("number");
      expect(event.durationMs as number).toBeGreaterThanOrEqual(0);
      console.log(
        `${eventType} metrics durationMs=${event.durationMs}ms sandboxId=${event.sandboxId}`
      );
    }
  },
  5 * 60_000
);
