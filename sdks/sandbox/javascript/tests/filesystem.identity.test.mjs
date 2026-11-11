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
import { Sandbox } from "../dist/index.js";
import { FilesystemAdapter, createExecdClient } from "../dist/internal.js";

const BASE_URL = "http://localhost:44772/sandboxes/example/port/44772";
const PREFIX = "/sandboxes/example/port/44772";

function makeAdapter(requests, status = 200, responseBody = "{}", errorBody = "unsupported") {
  const opts = {
    baseUrl: BASE_URL,
    headers: { "X-EXECD-ACCESS-TOKEN": "secret", "X-Routing": "sandbox" },
    fetch: async (input, init) => {
      const request = input instanceof Request ? input : new Request(input, init);
      requests.push({
        path: new URL(request.url).pathname,
        headers: request.headers,
        body: await request.arrayBuffer(),
      });
      return new Response(status === 200 ? responseBody : errorBody, {
        status,
        headers: { "content-type": "application/json" },
      });
    },
  };
  return new FilesystemAdapter(createExecdClient(opts), opts);
}

test("identity clients preserve proxy paths and headers without mutating the original", async () => {
  const requests = [];
  const original = makeAdapter(requests);
  const sandbox = { files: original };
  const a = Sandbox.prototype.filesWithIdentity.call(sandbox, 1001, 2000);
  const b = Sandbox.prototype.filesWithIdentity.call(sandbox, 1002, 2000);
  await Promise.all([a.getFileInfo(["/a"]), b.getFileInfo(["/b"])]);
  await a.readBytes("/file");
  await a.writeFiles([{ path: "/file", data: new Uint8Array([0, 255, 1]) }]);
  await original.getFileInfo(["/original"]);
  assert.deepEqual(new Set(requests.slice(0, 2).map(r => r.path)), new Set([
    PREFIX + "/v1/filesystem/1001/2000/files/info",
    PREFIX + "/v1/filesystem/1002/2000/files/info",
  ]));
  assert.equal(requests[2].path, PREFIX + "/v1/filesystem/1001/2000/files/download");
  assert.equal(requests[3].path, PREFIX + "/v1/filesystem/1001/2000/files/upload");
  assert.equal(requests[4].path, PREFIX + "/files/info");
  for (const request of requests) {
    assert.equal(request.headers.get("X-EXECD-ACCESS-TOKEN"), "secret");
    assert.equal(request.headers.get("X-Routing"), "sandbox");
  }
});

for (const status of [404, 501, 503]) {
  test("identity requests never fall back after HTTP " + status, async () => {
    for (const [operation, expectedPath] of [
      [files => files.writeFiles([{ path: "/file", data: "content" }]), "/files/upload"],
      [files => files.getFileInfo(["/file"]), "/files/info"],
      [files => files.replaceContents([{ path: "/file", oldContent: "a", newContent: "b" }]), "/files/replace"],
    ]) {
      const requests = [];
      const scoped = makeAdapter(requests, status).withIdentity(1001, 2000);
      await assert.rejects(operation(scoped));
      assert.equal(requests.length, 1);
      assert.equal(requests[0].path, PREFIX + "/v1/filesystem/1001/2000" + expectedPath);
    }
  });
}

for (const status of [404, 501, 503]) {
  test("identity requests reject empty-bodied failures after HTTP " + status, async () => {
    for (const operation of [
      files => files.replaceContents([{ path: "/file", oldContent: "a", newContent: "b" }]),
      files => files.replaceContentsDetailed([{ path: "/file", oldContent: "a", newContent: "b" }]),
    ]) {
      const requests = [];
      const scoped = makeAdapter(requests, status, "{}", "").withIdentity(1001, 2000);
      await assert.rejects(
        operation(scoped),
        error => error.name === "SandboxApiException" &&
          error.statusCode === status &&
          error.error.code === "UNEXPECTED_RESPONSE"
      );
      assert.equal(requests.length, 1);
      assert.equal(requests[0].path, PREFIX + "/v1/filesystem/1001/2000/files/replace");
    }
  });
}

test("replace failures keep structured error payloads from JSON bodies", async () => {
  const requests = [];
  const scoped = makeAdapter(requests, 400, "{}", '{"code":"INVALID_REQUEST_BODY","message":"bad request"}')
    .withIdentity(1001, 2000);
  await assert.rejects(
    scoped.replaceContents([{ path: "/file", oldContent: "a", newContent: "b" }]),
    error => error.name === "SandboxApiException" &&
      error.statusCode === 400 &&
      error.error.code === "INVALID_REQUEST_BODY" &&
      error.message === "bad request"
  );
  assert.equal(requests.length, 1);
});

test("replace contents accepts an empty successful response", async () => {
  const requests = [];
  const scoped = makeAdapter(requests, 200, "").withIdentity(1001, 2000);
  await scoped.replaceContents([{ path: "/file", oldContent: "a", newContent: "b" }]);
  assert.deepEqual(await scoped.replaceContentsDetailed([{ path: "/file", oldContent: "a", newContent: "b" }]), []);
  assert.equal(requests[0].path, PREFIX + "/v1/filesystem/1001/2000/files/replace");
});

test("invalid identities fail before sending requests", () => {
  const requests = [];
  const adapter = makeAdapter(requests);
  for (const value of [-1, 4294967295, 1.5, NaN, Infinity, true, "1001", null]) {
    assert.throws(() => adapter.withIdentity(value, 1000), RangeError);
    assert.throws(() => adapter.withIdentity(1000, value), RangeError);
  }
  assert.equal(requests.length, 0);
});

for (const responseBody of ['{"/file":', '<html>Bad gateway</html>']) {
  test("detailed replace reports malformed successful responses: " + responseBody, async () => {
    const requests = [];
    const scoped = makeAdapter(requests, 200, responseBody).withIdentity(1001, 2000);
    await assert.rejects(
      scoped.replaceContentsDetailed([{ path: "/file", oldContent: "a", newContent: "b" }]),
      error => !(error instanceof SyntaxError) &&
        error.message.startsWith("Replace contents failed: unexpected response shape (")
    );
    assert.equal(requests.length, 1);
    assert.equal(requests[0].path, PREFIX + "/v1/filesystem/1001/2000/files/replace");
  });
}

test("detailed replace preserves replacement counts", async () => {
  const requests = [];
  const scoped = makeAdapter(requests, 200, '{"/file":{"replacedCount":2}}').withIdentity(1001, 2000);
  assert.deepEqual(
    await scoped.replaceContentsDetailed([{ path: "/file", oldContent: "a", newContent: "b" }]),
    [{ path: "/file", replacedCount: 2 }]
  );
});

for (const responseBody of ['5', '0', 'true', 'false', 'null', '"value"', '[]', '[{"replacedCount":2}]']) {
  test("detailed replace rejects non-object responses: " + responseBody, async () => {
    const requests = [];
    const scoped = makeAdapter(requests, 200, responseBody).withIdentity(1001, 2000);
    await assert.rejects(
      scoped.replaceContentsDetailed([{ path: "/file", oldContent: "a", newContent: "b" }]),
      /Replace contents failed: unexpected response shape \(expected object\)/
    );
    assert.equal(requests.length, 1);
  });
}

for (const responseBody of ['{"/file":{"count":2}}', '{"/file":5}', '{"/file":null}', '{"/file":{"replacedCount":"2"}}']) {
  test("detailed replace rejects entries without a numeric replacedCount: " + responseBody, async () => {
    const requests = [];
    const scoped = makeAdapter(requests, 200, responseBody).withIdentity(1001, 2000);
    await assert.rejects(
      scoped.replaceContentsDetailed([{ path: "/file", oldContent: "a", newContent: "b" }]),
      /Replace contents failed: unexpected response shape \(invalid entry for \/file\)/
    );
    assert.equal(requests.length, 1);
  });
}

test("identity boundary values are accepted and validation reports the bound", async () => {
  const requests = [];
  const original = makeAdapter(requests);
  await original.withIdentity(0, 4294967294).getFileInfo(["/file"]);
  await original.withIdentity(4294967294, 0).getFileInfo(["/file"]);
  assert.equal(requests[0].path, PREFIX + "/v1/filesystem/0/4294967294/files/info");
  assert.equal(requests[1].path, PREFIX + "/v1/filesystem/4294967294/0/files/info");
  assert.throws(() => original.withIdentity(4294967295, 0), /uid must be an integer between 0 and 4294967294/);
  assert.throws(() => original.withIdentity(0, 4294967295), /gid must be an integer between 0 and 4294967294/);
});

test("custom adapters without identity support fail explicitly", () => {
  assert.throws(() => Sandbox.prototype.filesWithIdentity.call({ files: {} }, 1, 1), /does not support/);
});
