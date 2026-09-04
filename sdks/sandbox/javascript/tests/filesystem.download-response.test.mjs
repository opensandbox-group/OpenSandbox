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

import { FilesystemAdapter, createExecdClient } from "../dist/internal.js";

const baseUrl = "http://execd.test";

function createAdapter(fetch) {
  const client = createExecdClient({ baseUrl, fetch });
  return new FilesystemAdapter(client, { baseUrl, fetch });
}

async function collect(stream) {
  const chunks = [];
  for await (const chunk of stream) chunks.push(...chunk);
  return new Uint8Array(chunks);
}

test("readBytesDetailed exposes partial response metadata", async () => {
  const adapter = createAdapter(async (input, init) => {
    assert.equal(new URL(input).pathname, "/files/download");
    assert.equal(new Headers(init.headers).get("range"), "bytes=0-4");
    return new Response("hello", {
      status: 206,
      headers: {
        "Content-Type": "application/octet-stream",
        "Content-Disposition": 'attachment; filename="data.bin"',
        "Content-Length": "5",
        "Content-Range": "bytes 0-4/10",
      },
    });
  });

  const response = await adapter.readBytesDetailed("/data.bin", {
    range: "bytes=0-4",
  });

  assert.deepEqual(response.body, new TextEncoder().encode("hello"));
  assert.equal(response.statusCode, 206);
  assert.equal(response.isPartial, true);
  assert.equal(response.contentType, "application/octet-stream");
  assert.equal(response.contentDisposition, 'attachment; filename="data.bin"');
  assert.equal(response.contentLength, 5);
  assert.equal(response.totalSize, 10);
  assert.deepEqual(response.contentRange, {
    start: 0,
    end: 4,
    total: 10,
    raw: "bytes 0-4/10",
  });
});

test("readBytesDetailed identifies an ignored Range request", async () => {
  const adapter = createAdapter(async () =>
    new Response("whole file", {
      status: 200,
      headers: { "Content-Length": "10" },
    }),
  );

  const response = await adapter.readBytesDetailed("/data.bin", {
    range: "bytes=5-",
  });

  assert.equal(response.statusCode, 200);
  assert.equal(response.isPartial, false);
  assert.equal(response.contentRange, undefined);
  assert.equal(response.contentLength, 10);
  assert.equal(response.totalSize, 10);
});

test("readBytesDetailed identifies a partial response without Content-Range", async () => {
  const adapter = createAdapter(async () =>
    new Response("hello", {
      status: 206,
    }),
  );

  const response = await adapter.readBytesDetailed("/data.bin");

  assert.equal(response.isPartial, true);
  assert.equal(response.contentRange, undefined);
  assert.equal(response.totalSize, -1);
});

test("readBytesDetailed preserves malformed and unknown Content-Range values", async () => {
  const ranges = ["garbage", "bytes 5-9/*"];
  const adapter = createAdapter(async () =>
    new Response("hello", {
      status: 206,
      headers: { "Content-Range": ranges.shift() },
    }),
  );

  const malformed = await adapter.readBytesDetailed("/data.bin");
  assert.deepEqual(malformed.contentRange, {
    start: -1,
    end: -1,
    total: -1,
    raw: "garbage",
  });
  assert.equal(malformed.totalSize, -1);

  const unknown = await adapter.readBytesDetailed("/data.bin");
  assert.deepEqual(unknown.contentRange, {
    start: 5,
    end: 9,
    total: -1,
    raw: "bytes 5-9/*",
  });
  assert.equal(unknown.totalSize, -1);
});

test("detailed and legacy streaming reads return the response body", async () => {
  const adapter = createAdapter(async () =>
    new Response("hello", {
      status: 206,
      headers: {
        "Content-Length": "5",
        "Content-Range": "bytes 0-4/10",
      },
    }),
  );

  const detailed = await adapter.readBytesStreamDetailed("/data.bin", {
    range: "bytes=0-4",
  });
  assert.equal(detailed.isPartial, true);
  assert.deepEqual(await collect(detailed.body), new TextEncoder().encode("hello"));

  assert.deepEqual(
    await adapter.readBytes("/data.bin"),
    new TextEncoder().encode("hello"),
  );
  assert.deepEqual(
    await collect(adapter.readBytesStream("/data.bin")),
    new TextEncoder().encode("hello"),
  );
});

test("detailed streams cancel unconsumed and partially consumed bodies", async () => {
  const cancelled = [];
  const adapter = createAdapter(async () => {
    const index = cancelled.length;
    cancelled.push(false);
    return new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode("hello"));
        },
        cancel() {
          cancelled[index] = true;
        },
      }),
      { status: 206, headers: { "Content-Range": "bytes 0-4/10" } },
    );
  });

  const unconsumed = await adapter.readBytesStreamDetailed("/data.bin");
  await unconsumed.body.close();
  assert.equal(cancelled[0], true);

  const partial = await adapter.readBytesStreamDetailed("/data.bin");
  for await (const _chunk of partial.body) {
    break;
  }
  assert.equal(cancelled[1], true);
});
