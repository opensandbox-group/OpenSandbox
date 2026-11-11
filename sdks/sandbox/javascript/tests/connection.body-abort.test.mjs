// Copyright 2026 The OpenSandbox Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import assert from "node:assert/strict";
import { once } from "node:events";
import { createServer } from "node:http";
import { setTimeout as sleep } from "node:timers/promises";
import test from "node:test";
import { ConnectionConfig } from "../dist/index.js";
import { _setClientIpForTest } from "../dist/internal.js";

async function streamingServer(t) {
  const server = createServer((_req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    res.write('data: {"type":"stdout","text":"first"}\n\n');
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(async () => {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  });
  return `http://127.0.0.1:${server.address().port}`;
}

for (const transport of ["fetch", "sseFetch", "uploadFetch"]) {
  for (const signalOnRequest of [false, true]) {
    test(`${transport}: abort after headers cancels body (${signalOnRequest ? "Request" : "init"} signal)`, { timeout: 5000 }, async (t) => {
      _setClientIpForTest("127.0.0.1");
      const url = await streamingServer(t);
      const config = new ConnectionConfig({ wakeRetryEnabled: false }).withTransportIfMissing();
      t.after(() => config.closeTransport());
      const controller = new AbortController();
      const response = signalOnRequest
        ? await config[transport](new Request(url, { signal: controller.signal }))
        : await config[transport](url, { signal: controller.signal });
      const reader = response.body.getReader();
      t.after(() => reader.cancel().catch(() => {}));
      assert.equal((await reader.read()).done, false);
      const pendingRead = reader.read().then(
        () => "resolved",
        (error) => error.name,
      );
      controller.abort();
      let timer;
      try {
        const result = await Promise.race([
          pendingRead,
          new Promise((resolve) => { timer = setTimeout(() => resolve("not cancelled"), 500); }),
        ]);
        assert.equal(result, "AbortError");
      } finally {
        clearTimeout(timer);
      }
    });
  }
}

test("response-header timeout does not become a response-body timeout", { timeout: 5000 }, async (t) => {
  _setClientIpForTest("127.0.0.1");
  const url = await streamingServer(t);
  const config = new ConnectionConfig({ requestTimeoutSeconds: 0.1, wakeRetryEnabled: false }).withTransportIfMissing();
  t.after(() => config.closeTransport());
  const response = await config.fetch(url);
  const reader = response.body.getReader();
  t.after(() => reader.cancel().catch(() => {}));
  await reader.read();
  let settled = false;
  const pendingRead = reader.read().finally(() => { settled = true; });
  await sleep(200);
  assert.equal(settled, false, "the response must remain open beyond the header deadline");
  await reader.cancel();
  await pendingRead;
});
