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

import { Sandbox, SandboxReadyTimeoutException } from "@alibaba-group/opensandbox";

test("waitUntilReady timeout includes last health-check error and connection context", async () => {
  const fakeSandbox = {
    connectionConfig: {
      domain: "localhost:8080",
      useServerProxy: false,
    },
    health: {
      ping: async () => {
        throw new Error("connect ECONNREFUSED 127.0.0.1:8080");
      },
    },
  } as unknown as Sandbox;

  let thrown: unknown;
  try {
    await Sandbox.prototype.waitUntilReady.call(fakeSandbox, {
      readyTimeoutSeconds: 0.01,
      pollingIntervalMillis: 1,
    });
  } catch (err) {
    thrown = err;
  }

  expect(thrown).toBeInstanceOf(SandboxReadyTimeoutException);
  const message = (thrown as Error).message;
  expect(message).toContain("Sandbox health check timed out");
  expect(message).toContain("Last health check error");
  expect(message).toContain("domain=localhost:8080");
  expect(message).toContain("useServerProxy=false");
  expect(message).toContain("useServerProxy=true");
});

test("waitUntilReady timeout includes false-continuously hint when ping returns false", async () => {
  let pingCalls = 0;
  const fakeSandbox = {
    connectionConfig: {
      domain: "localhost:8080",
      useServerProxy: true,
    },
    health: {
      ping: async () => {
        pingCalls++;
        return false;
      },
    },
  } as unknown as Sandbox;

  let thrown: unknown;
  try {
    await Sandbox.prototype.waitUntilReady.call(fakeSandbox, {
      readyTimeoutSeconds: 0.01,
      pollingIntervalMillis: 1,
    });
  } catch (err) {
    thrown = err;
  }

  expect(thrown).toBeInstanceOf(SandboxReadyTimeoutException);
  expect((thrown as Error).message).toContain("Health check returned false continuously.");
  expect(pingCalls).toBeGreaterThan(0);
});
