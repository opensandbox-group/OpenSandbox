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

import assert from "node:assert/strict";
import test from "node:test";

import { ConnectionConfig, InvalidArgumentException, Sandbox } from "../dist/index.js";

function createAdapterFactory() {
  const calls = [];
  const sandboxes = {
    async createSandbox() {
      calls.push("createSandbox");
      return { id: "sbx-1", expiresAt: null };
    },
    async getSandboxEndpoint(sandboxId, port) {
      calls.push("getSandboxEndpoint");
      return { endpoint: `sandbox.internal:${port}`, headers: {} };
    },
    async resumeSandbox() {
      calls.push("resumeSandbox");
    },
    async getSandbox() {
      throw new Error("not implemented");
    },
    async deleteSandbox() {
      calls.push("deleteSandbox");
    },
  };
  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack() {
      return {
        commands: {},
        files: {},
        health: {
          async ping() {
            calls.push("ping");
            return false;
          },
        },
        metrics: {},
      };
    },
    createEgressStack() {
      return { egress: {} };
    },
  };
  return { adapterFactory, calls };
}

function connectionConfig() {
  const config = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  config.withTransportIfMissing = () => config;
  return config;
}

const operations = {
  create: (adapterFactory, interval) =>
    Sandbox.create({
      adapterFactory,
      connectionConfig: connectionConfig(),
      image: "python:3.12",
      readyTimeoutSeconds: 1,
      healthCheckPollingInterval: interval,
    }),
  connect: (adapterFactory, interval) =>
    Sandbox.connect({
      sandboxId: "sbx-1",
      adapterFactory,
      connectionConfig: connectionConfig(),
      readyTimeoutSeconds: 1,
      healthCheckPollingInterval: interval,
    }),
  resume: (adapterFactory, interval) =>
    Sandbox.resume({
      sandboxId: "sbx-1",
      adapterFactory,
      connectionConfig: connectionConfig(),
      readyTimeoutSeconds: 1,
      healthCheckPollingInterval: interval,
    }),
};

for (const [name, run] of Object.entries(operations)) {
  test(`Sandbox.${name} rejects a negative health check polling interval before any request`, async () => {
    const { adapterFactory, calls } = createAdapterFactory();

    await assert.rejects(run(adapterFactory, -1), InvalidArgumentException);
    assert.deepEqual(calls, []);
  });
}

test("Sandbox.create ignores the polling interval when the health check is skipped", async () => {
  const { adapterFactory, calls } = createAdapterFactory();

  await Sandbox.create({
    adapterFactory,
    connectionConfig: connectionConfig(),
    image: "python:3.12",
    skipHealthCheck: true,
    healthCheckPollingInterval: -1,
  });

  assert.deepEqual(calls, ["createSandbox", "getSandboxEndpoint", "getSandboxEndpoint"]);
});
