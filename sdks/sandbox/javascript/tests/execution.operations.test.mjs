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

import assert from "node:assert/strict";
import test from "node:test";
import { CommandsAdapter, createExecdClient } from "../dist/internal.js";
import { newOperationId, SandboxApiException, getExecutionOperations } from "../dist/index.js";

test("operation requests preserve identity, map fields, and do not claim execution success", async () => {
  const requests = [];
  const fetchImpl = async (request) => {
    requests.push(request);
    const url = new URL(request.url);
    if (url.pathname === "/execution/instance") return Response.json({ instance_id: "scope", issued_at: 123, retention_seconds: 86400, capacity: 4096 });
    if (request.method === "POST") {
      const body = await request.json();
      assert.equal(body.operation_id, "scope.123.persisted");
      assert.equal(body.command, "echo hello");
      assert.equal(body.cwd, "/tmp");
      if (url.pathname === "/command/operations") { assert.equal(body.timeout, 2000); assert.deepEqual(body.envs, { A: "value" }); }
    } else {
      assert.equal(request.headers.get("X-EXECD-OPERATION-ID"), "scope.123.persisted");
      if (url.searchParams.get("kind") === "pty") return Response.json({ code: "operation_instance_mismatch", message: "unknown outcome" }, { status: 409 });
    }
    return Response.json({ id: "original", kind: "command", state: "creating", expires_at: "2026-09-09T00:00:00Z" }, { status: 202 });
  };
  const client = createExecdClient({ baseUrl: "http://localhost:44772", fetch: fetchImpl });
  const adapter = new CommandsAdapter(client, { baseUrl: "http://localhost:44772", fetch: fetchImpl });
  assert.equal(getExecutionOperations(adapter), adapter);
  const instance = await getExecutionOperations(adapter).getExecutionInstance();
  assert.match(newOperationId(instance), /^scope\.123\.[a-f0-9]{32}$/);
  const op = await adapter.createCommandOperation("scope.123.persisted", "echo hello", { workingDirectory: "/tmp", timeoutSeconds: 2, envs: { A: "value" } });
  assert.equal(op.state, "creating");
  assert.equal((await adapter.getExecutionOperation("command", "scope.123.persisted")).id, op.id);
  assert.equal((await adapter.createPTYOperation("scope.123.persisted", { cwd: "/tmp", command: "echo hello" })).id, op.id);
  await assert.rejects(adapter.getExecutionOperation("pty", "scope.123.persisted"), SandboxApiException);
  assert.equal(requests.length, 5);
});


test("legacy adapters retain type compatibility and recovery is optional", async () => {
  const { execFileSync } = await import("node:child_process");
  const { fileURLToPath } = await import("node:url");
  execFileSync(process.execPath, [fileURLToPath(new URL("../node_modules/typescript/bin/tsc", import.meta.url)),
    "--noEmit", "--strict", "--skipLibCheck", "--target", "ES2022", "--module", "NodeNext", "--moduleResolution", "NodeNext",
    fileURLToPath(new URL("./legacy-commands.mts", import.meta.url))]);
  assert.throws(() => getExecutionOperations({}), /does not support/);
});

function instanceAdapter(fetch) {
  const config = { baseUrl: "http://localhost:44772", fetch };
  return new CommandsAdapter(createExecdClient(config), config);
}

const instanceBody = (issuedAt = 123) => ({ instance_id: "scope", issued_at: issuedAt, retention_seconds: 86400, capacity: 4096 });

test("instance cache coalesces requests, isolates snapshots and expires from fetch start", async (t) => {
  let now = 0;
  t.mock.method(performance, "now", () => now);
  let calls = 0;
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  const adapter = instanceAdapter(async () => {
    const issuedAt = ++calls;
    await gate;
    return Response.json(instanceBody(issuedAt));
  });
  const pending = Array.from({ length: 16 }, () => adapter.getExecutionInstance());
  now = 59_000;
  release();
  const values = await Promise.all(pending);
  assert.equal(calls, 1);
  assert.equal(new Set(values).size, 16);
  values[0].instance_id = "caller-mutation";
  assert.equal((await adapter.getExecutionInstance()).instance_id, "scope");
  now = 60_000;
  assert.equal((await adapter.getExecutionInstance()).issued_at, 2);
  assert.equal(calls, 2);
});

test("failed instance fetch is retried by the next caller", async () => {
  let calls = 0;
  const adapter = instanceAdapter(async () => ++calls === 1
    ? Response.json({ code: "unavailable", message: "retry later" }, { status: 503 })
    : Response.json(instanceBody()));
  await assert.rejects(adapter.getExecutionInstance(), SandboxApiException);
  assert.equal((await adapter.getExecutionInstance()).instance_id, "scope");
  assert.equal(calls, 2);
});

for (const code of ["operation_instance_mismatch", "operation_expired"]) {
  for (const method of ["command", "pty", "lookup"]) {
    test(`${code} during ${method} invalidates inflight instance without replay`, async () => {
      let gets = 0;
      let operations = 0;
      let release;
      let entered;
      const gate = new Promise((resolve) => { release = resolve; });
      const started = new Promise((resolve) => { entered = resolve; });
      const adapter = instanceAdapter(async (request) => {
        if (new URL(request.url).pathname === "/execution/instance") {
          const issuedAt = ++gets;
          if (issuedAt === 1) { entered(); await gate; }
          return Response.json(instanceBody(issuedAt));
        }
        operations++;
        if (request.method === "POST") assert.equal((await request.json()).operation_id, "saved.identity");
        else assert.equal(request.headers.get("X-EXECD-OPERATION-ID"), "saved.identity");
        return Response.json({ code, message: "unknown outcome" }, { status: 409 });
      });
      const old = adapter.getExecutionInstance();
      await started;
      const action = method === "command" ? adapter.createCommandOperation("saved.identity", "true")
        : method === "pty" ? adapter.createPTYOperation("saved.identity")
        : adapter.getExecutionOperation("command", "saved.identity");
      await assert.rejects(action, SandboxApiException);
      assert.equal((await adapter.getExecutionInstance()).issued_at, 2);
      release();
      assert.equal((await old).issued_at, 1);
      assert.equal((await adapter.getExecutionInstance()).issued_at, 2);
      assert.equal(gets, 2);
      assert.equal(operations, 1);
    });
  }
}
