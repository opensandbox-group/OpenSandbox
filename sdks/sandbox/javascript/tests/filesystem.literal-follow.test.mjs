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

import { FilesystemAdapter, createExecdClient } from "../dist/internal.js";

const baseUrl = "http://execd.test";

function createCaptureAdapter(seen) {
  const fetch = async (input) => {
    seen.push(typeof input === "string" ? input : input.url);
    return new Response("payload", {
      status: 200,
      headers: { "Content-Length": "7", "Content-Type": "application/octet-stream" },
    });
  };
  const client = createExecdClient({ baseUrl, fetch });
  return new FilesystemAdapter(client, { baseUrl, fetch });
}

test("download literal option maps to a query parameter", async () => {
  const seen = [];
  const adapter = createCaptureAdapter(seen);

  await adapter.readBytesDetailed("/data-$V.txt", { literal: true });
  assert.match(seen.at(-1), /literal=true/);
  assert.match(seen.at(-1), /path=%2Fdata-%24V\.txt/);

  // Omitted by default so the server keeps its expansion behavior.
  await adapter.readBytesDetailed("/data-$V.txt");
  assert.doesNotMatch(seen.at(-1), /literal=/);

  await adapter.readBytesDetailed("/data-$V.txt", { literal: false });
  assert.match(seen.at(-1), /literal=false/);
});

test("getFileInfo literal and follow map to query parameters", async () => {
  const seen = [];
  const fetch = async (input) => {
    seen.push(typeof input === "string" ? input : input.url);
    return new Response("{}", {
      status: 200,
      headers: { "Content-Type": "application/json" },
    });
  };
  const client = createExecdClient({ baseUrl, fetch });
  const adapter = new FilesystemAdapter(client, { baseUrl, fetch });

  await adapter.getFileInfo(["/a.txt"], { literal: true, follow: true });
  assert.match(seen.at(-1), /literal=true/);
  assert.match(seen.at(-1), /follow=true/);

  await adapter.getFileInfo(["/a.txt"]);
  assert.doesNotMatch(seen.at(-1), /literal=/);
  assert.doesNotMatch(seen.at(-1), /follow=/);
});
