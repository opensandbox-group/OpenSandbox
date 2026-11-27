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

import { ConnectionConfig } from "../dist/index.js";
import { _setClientIpForTest } from "../dist/internal.js";

test("ConnectionConfig strips trailing slash suffix without regex backtracking", () => {
  const connectionConfig = new ConnectionConfig({
    domain: `https://api.opensandbox.test${"/".repeat(4096)}`,
  });

  assert.equal(connectionConfig.getBaseUrl(), "https://api.opensandbox.test/v1");
});

test("ConnectionConfig preserves path prefix while normalizing v1 suffix", () => {
  const connectionConfig = new ConnectionConfig({
    domain: "https://api.opensandbox.test/proxy/v1/",
  });

  assert.equal(connectionConfig.getBaseUrl(), "https://api.opensandbox.test/proxy/v1");
});

// Regression: the default User-Agent version is hand-maintained and must be
// bumped together with the package version. Update this expectation when releasing.
test("ConnectionConfig default userAgent matches package version", () => {
  const connectionConfig = new ConnectionConfig({
    domain: "https://api.opensandbox.test",
  });

  assert.equal(connectionConfig.userAgent, "OpenSandbox-JS-SDK/1.2.1");
});

test("ConnectionConfig.disableMetrics is preserved by withTransportIfMissing", () => {
  const connectionConfig = new ConnectionConfig({
    domain: "https://api.opensandbox.test",
    disableMetrics: true,
  });
  assert.equal(connectionConfig.disableMetrics, true);
  assert.equal(connectionConfig.withTransportIfMissing().disableMetrics, true);
});

test("ConnectionConfig does not duplicate an API key header supplied in another casing", () => {
  const connectionConfig = new ConnectionConfig({
    domain: "https://api.opensandbox.test",
    apiKey: "tenant-secret",
    headers: { "open-sandbox-api-key": "explicit-secret" },
  });

  const apiKeyHeaders = Object.keys(connectionConfig.headers).filter(
    (key) => key.toLowerCase() === "open-sandbox-api-key",
  );
  assert.deepEqual(apiKeyHeaders, ["open-sandbox-api-key"]);
  assert.equal(connectionConfig.headers["open-sandbox-api-key"], "explicit-secret");
});

test("ConnectionConfig debug fetch redacts X-EXECD-ACCESS-TOKEN in logs", async () => {
  _setClientIpForTest("127.0.0.1");
  const secretToken = "execd-super-secret-token";
  const logged = [];
  const origLog = console.log;
  console.log = (...args) =>
    logged.push(args.map((a) => (typeof a === "string" ? a : JSON.stringify(a))).join(" "));
  const connectionConfig = new ConnectionConfig({
    domain: "http://127.0.0.1:1",
    debug: true,
    wakeRetryEnabled: false,
  }).withTransportIfMissing();
  try {
    try {
      await connectionConfig.fetch("http://127.0.0.1:1/v1/test", {
        headers: {
          "X-EXECD-ACCESS-TOKEN": secretToken,
          "X-Custom": "visible",
        },
      });
    } catch {
      // Expected connection refusal; outgoing request headers are logged before dispatch.
    }
  } finally {
    console.log = origLog;
    await connectionConfig.closeTransport();
  }

  const combined = logged.join("\n");
  assert.ok(!combined.includes(secretToken), "debug log must not leak X-EXECD-ACCESS-TOKEN");
  assert.ok(combined.includes('"***"'), "debug log must redact X-EXECD-ACCESS-TOKEN");
  assert.ok(combined.includes("visible"), "debug log must preserve non-sensitive headers");
});
