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
import { once } from "node:events";
import { createServer } from "node:http";
import test from "node:test";
import { SandboxManager } from "../dist/index.js";

for (const [method, path] of [
  ["listSandboxInfos", "sandboxes"],
  ["listSnapshots", "snapshots"],
  ["listTemplates", "templates"],
]) {
  test(`${method} validates pagination and preserves valid requests`, async () => {
    const requests = [];
    const server = createServer((req, res) => {
      requests.push(req.url);
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ items: [] }));
    });
    server.listen(0, "127.0.0.1");
    await once(server, "listening");
    const manager = SandboxManager.create({
      connectionConfig: { domain: `127.0.0.1:${server.address().port}`, protocol: "http" },
    });
    try {
      for (const field of ["page", "pageSize"]) {
        for (const value of [0, -1, 1.5, NaN, Infinity]) {
          await assert.rejects(
            manager[method]({ [field]: value }),
            { name: "RangeError", message: `${field} must be an integer greater than or equal to 1` },
          );
        }
      }
      assert.deepEqual(requests, []);
      await manager[method]();
      await manager[method]({ page: 1, pageSize: 200 });
      assert.deepEqual(requests, [`/v1/${path}`, `/v1/${path}?page=1&pageSize=200`]);
    } finally {
      await manager.close();
      await new Promise((resolve, reject) => {
        server.close((err) => err ? reject(err) : resolve());
      });
    }
  });
}
