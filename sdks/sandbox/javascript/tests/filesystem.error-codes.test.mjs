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

import { SandboxApiException } from "../dist/index.js";
import {
  FilesystemAdapter,
  IsolatedFilesystemAdapter,
  createExecdClient,
} from "../dist/internal.js";

const baseUrl = "http://execd.test";
const errorBody = JSON.stringify({ code: "FILE_NOT_FOUND", message: "file not found: /missing" });

function failingFetch(status) {
  return async () =>
    new Response(errorBody, {
      status,
      headers: { "Content-Type": "application/json", "x-request-id": "req-1" },
    });
}

function adapters(fetch) {
  const opts = { baseUrl, fetch };
  return {
    execd: new FilesystemAdapter(createExecdClient(opts), opts),
    isolated: new IsolatedFilesystemAdapter(createExecdClient(opts), { ...opts, sessionId: "s1" }),
  };
}

function assertStructured(status) {
  return (err) => {
    assert.ok(err instanceof SandboxApiException);
    assert.equal(err.statusCode, status);
    assert.equal(err.requestId, "req-1");
    assert.equal(err.error.code, "FILE_NOT_FOUND");
    assert.equal(err.message, "file not found: /missing");
    assert.equal(err.rawBody, errorBody);
    return true;
  };
}

for (const kind of ["execd", "isolated"]) {
  test(`${kind} readBytes keeps the server error code`, async () => {
    const files = adapters(failingFetch(404))[kind];
    await assert.rejects(files.readBytes("/missing"), assertStructured(404));
  });

  test(`${kind} readBytesStream keeps the server error code`, async () => {
    const files = adapters(failingFetch(404))[kind];
    await assert.rejects(
      (async () => {
        for await (const _ of files.readBytesStream("/missing")) {
          // drain
        }
      })(),
      assertStructured(404),
    );
  });

  test(`${kind} writeFiles keeps the server error code`, async () => {
    const files = adapters(failingFetch(403))[kind];
    await assert.rejects(
      files.writeFiles([{ path: "/missing", data: "x" }]),
      assertStructured(403),
    );
  });
}

test("download failure without a body falls back to the adapter message", async () => {
  const files = adapters(async () => new Response("", { status: 500 })).execd;
  await assert.rejects(files.readBytes("/x"), (err) => {
    assert.ok(err instanceof SandboxApiException);
    assert.equal(err.statusCode, 500);
    assert.equal(err.error.code, "UNEXPECTED_RESPONSE");
    assert.equal(err.message, "Download failed");
    return true;
  });
});
