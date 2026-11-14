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
import { Buffer } from "node:buffer";
import test from "node:test";

import {
  FilesystemAdapter,
  IsolatedSessionsAdapter,
  createExecdClient,
} from "../dist/internal.js";

const BASE_URL = "http://127.0.0.1:8080";

async function withBrowserRuntime(run) {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, "process");
  Object.defineProperty(globalThis, "process", { configurable: true, value: undefined });
  try {
    return await run();
  } finally {
    Object.defineProperty(globalThis, "process", descriptor);
  }
}

async function createUploader(kind) {
  const uploads = [];
  const fetchImpl = async (input, init) => {
    const request = input instanceof Request ? input : new Request(input, init);
    if (request.method === "GET") {
      return Response.json({ created_at: "2026-01-01T00:00:00Z" });
    }
    const form = await request.formData();
    uploads.push({
      path: new URL(request.url).pathname,
      metadata: JSON.parse(await form.get("metadata").text()),
      bytes: new Uint8Array(await form.get("file").arrayBuffer()),
    });
    return Response.json({});
  };
  const opts = { baseUrl: BASE_URL, fetch: fetchImpl };
  const files = kind === "isolated"
    ? (await new IsolatedSessionsAdapter(opts).attach("session-1")).files
    : new FilesystemAdapter(createExecdClient(opts), opts);
  return {
    uploads,
    upload(data) {
      const run = () => files.writeFiles([{ path: "/tmp/chunks.bin", data }]);
      return kind === "browser" ? withBrowserRuntime(run) : run();
    },
  };
}

async function* reuseBuffer(view) {
  view.fill(65);
  yield view;
  view.fill(66);
  yield view;
}

function reusedReadableStream() {
  const view = new Uint8Array(4).subarray(1, 3);
  let count = 0;
  return new ReadableStream({
    pull(controller) {
      view.fill(count++ === 0 ? 65 : 66);
      controller.enqueue(view);
      if (count === 2) controller.close();
    },
  }, { highWaterMark: 0 });
}

// Node's test runner isolates files in separate processes and runs these tests
// serially. Only "browser" hides process to select the non-Node fallback; it
// exercises the real adapter and multipart body, not an actual browser engine.
for (const kind of ["browser", "isolated", "node"]) {
  for (const [name, source] of [
    ["Uint8Array subview", () => reuseBuffer(new Uint8Array(4).subarray(1, 3))],
    ["Buffer subview", () => reuseBuffer(Buffer.alloc(4).subarray(1, 3))],
    ["ReadableStream", reusedReadableStream],
  ]) {
    test(`${kind} upload snapshots a reused ${name}`, async () => {
      const { upload, uploads } = await createUploader(kind);
      await upload(source());
      assert.equal(uploads.length, 1);
      assert.equal(uploads[0].path, kind === "isolated"
        ? "/v1/isolated/session/session-1/files/upload" : "/files/upload");
      assert.equal(uploads[0].metadata.path, "/tmp/chunks.bin");
      assert.deepEqual(uploads[0].bytes, new Uint8Array([65, 65, 66, 66]));
    });
  }

  test(`${kind} upload preserves independent binary chunks and empty input`, async () => {
    const { upload, uploads } = await createUploader(kind);
    async function* independentChunks() {
      yield new Uint8Array([0, 255]);
      yield new Uint8Array([128, 1]);
    }
    await upload(independentChunks());
    async function* empty() { /* No data. */ }
    await upload(empty());
    assert.deepEqual(uploads.map(({ bytes }) => Array.from(bytes)), [[0, 255, 128, 1], []]);
  });

  // The Node path starts sending before the producer is exhausted; only
  // buffering guarantees producer errors occur before an upload request.
  if (kind === "node") continue;

  test(`${kind} buffered upload propagates producer errors before sending`, async () => {
    const { upload, uploads } = await createUploader(kind);
    const failure = new Error("producer failed");
    let finalized = false;
    async function* source() {
      try {
        yield new Uint8Array([1]);
        throw failure;
      } finally {
        finalized = true;
      }
    }
    await assert.rejects(() => upload(source()), (error) => error === failure);
    assert.equal(finalized, true);
    assert.deepEqual(uploads, []);
  });

  test(`${kind} buffered upload releases an errored stream reader`, async () => {
    const { upload, uploads } = await createUploader(kind);
    const failure = new Error("stream failed");
    let count = 0;
    const source = new ReadableStream({
      pull(controller) {
        if (count++ === 0) controller.enqueue(new Uint8Array([1]));
        else controller.error(failure);
      },
    }, { highWaterMark: 0 });
    await assert.rejects(() => upload(source), (error) => error === failure);
    assert.equal(source.locked, false);
    assert.deepEqual(uploads, []);
  });
}
