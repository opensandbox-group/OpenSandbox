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

// #2945 (JS half): a stalled health ping must fail after the per-ping cap
// (HEALTH_PING_TIMEOUT_SECONDS, mirroring the Python SDK's d8640951f) and
// let the readiness loop retry, instead of one stalled connection holding
// the whole readyTimeoutSeconds budget.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import net from "node:http";
import { HealthAdapter, HEALTH_PING_TIMEOUT_SECONDS } from "../dist/internal.js";
import { Sandbox } from "../dist/index.js";


function makeLifecycleApi(execdPort) {
  return net.createServer((req, res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ endpoint: `127.0.0.1:${execdPort}` }));
  });
}

test("a stalled first ping fails after the per-ping cap and readiness retries", async () => {
  // execd stand-in: a raw TCP server that holds the first connection open
  // without answering (the stalled ping), and answers every later one with
  // a bare 200 (the retry that succeeds). Raw sockets are what makes the
  // first connection genuinely stall for fetch.
  const execd = net.createServer();
  const seen = [];
  execd.on("connection", (sock) => {
    seen.push(sock);
    if (seen.length === 1) {
      return; // hold the first connection, never answer
    }
    sock.on("data", () => {});
    sock.write("HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
    sock.end();
  });

  await new Promise((r) => execd.listen(0, "127.0.0.1", r));
  const execdPort = execd.address().port;
  const api = makeLifecycleApi(execdPort);
  await new Promise((r) => api.listen(0, "127.0.0.1", r));
  const apiPort = api.address().port;

  const started = Date.now();
  try {
    const sbx = await Sandbox.connect({
      sandboxId: "sbx-ping",
      connectionConfig: {
        domain: `http://127.0.0.1:${apiPort}`,
        apiKey: "test",
        requestTimeoutSeconds: 30,
      },
      readyTimeoutSeconds: 30,
    });
    const elapsed = (Date.now() - started) / 1000;
    // The stalled ping must give up at the ~5s cap, not at the 30s budget.
    assert.ok(elapsed < HEALTH_PING_TIMEOUT_SECONDS + 5,
      `connect took ${elapsed}s; expected the stalled ping to fail at ~${HEALTH_PING_TIMEOUT_SECONDS}s and retry`);
    assert.ok(seen.length >= 2, `expected at least 2 execd connections, saw ${seen.length}`);
    await sbx.destroy?.();
  } finally {
    execd.close();
    api.close();
    for (const s of seen) s.destroy();
  }
});
