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

import { DefaultAdapterFactory } from "../dist/index.js";

function eventResponse(ending, framed, fragmented) {
  const events = [
    { type: "init", text: "exec-line-endings", timestamp: 1 },
    { type: "stdout", text: "你好\r\n", timestamp: 2 },
    { type: "execution_complete", timestamp: 3, execution_time: 4, exit_code: 0 },
  ];
  const wire = events.map((event) => `${framed ? "data: " : ""}${JSON.stringify(event)}`)
    .join(ending + (framed ? ending : ""));
  const bytes = new TextEncoder().encode(wire);
  return new Response(new ReadableStream({
    start(controller) {
      if (fragmented) {
        // Split CRLF pairs, JSON tokens and UTF-8 characters across reads.
        for (let index = 0; index < bytes.length; index += 1) {
          controller.enqueue(bytes.subarray(index, index + 1));
        }
      } else {
        controller.enqueue(bytes);
      }
      controller.close();
    },
  }), { headers: { "content-type": framed ? "text/event-stream" : "application/x-ndjson" } });
}

for (const [name, ending] of [["LF", "\n"], ["CRLF", "\r\n"], ["CR", "\r"]]) {
  for (const framed of [true, false]) {
    for (const fragmented of [true, false]) {
      test(`CodesAdapter.run parses ${framed ? "SSE" : "NDJSON"} ${name} lines (${fragmented ? "byte chunks" : "one chunk"})`, async () => {
        const fetchImpl = async () => eventResponse(ending, framed, fragmented);
        const adapter = new DefaultAdapterFactory().createCodes({
          sandbox: { connectionConfig: { headers: {}, fetch: fetchImpl, sseFetch: fetchImpl } },
          execdBaseUrl: "http://sandbox.invalid",
          endpointHeaders: {},
        });
        const execution = await adapter.run("print(1)");

        assert.equal(execution.id, "exec-line-endings");
        assert.deepEqual(execution.logs.stdout.map((entry) => entry.text), ["你好\r\n"]);
        assert.equal(execution.complete?.executionTimeMs, 4);
      });
    }
  }
}
