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

import { CommandsAdapter } from "../dist/internal.js";

const methods = {
  run: (adapter, options) => adapter.run("sleep 10", options),
  runStream: async (adapter, options) => {
    for await (const _event of adapter.runStream("sleep 10", options)) {
      // Consume the stream to send the request.
    }
  },
  runInSession: (adapter, options) => adapter.runInSession("session-1", "sleep 10", options),
  runInSessionStream: async (adapter, options) => {
    for await (const _event of adapter.runInSessionStream("session-1", "sleep 10", options)) {
      // Consume the stream to send the request.
    }
  },
};

function recordingAdapter(requests) {
  return new CommandsAdapter({}, {
    baseUrl: "http://sandbox.example",
    fetch: async (_url, init) => {
      requests.push(JSON.parse(init.body));
      return new Response('data: {"type":"execution_complete","timestamp":1}\n\n', {
        headers: { "content-type": "text/event-stream" },
      });
    },
  });
}

for (const [name, run] of Object.entries(methods)) {
  for (const [timeoutSeconds, expected] of [
    [Number.MIN_VALUE, 1],
    [0.000001, 1],
    [0.0004, 1],
    [0.0005, 1],
    [0.0009, 1],
    [0.001, 1],
    [0.0011, 1],
    [0.0015, 2],
    [0.0019, 2],
    [1.001, 1001],
    // Multiplication has a small positive floating-point error: do not use ceil.
    [2.007, 2007],
    [30, 30000],
    [0, 0],
  ]) {
    test(`${name} maps ${timeoutSeconds}s to ${expected}ms`, async () => {
      const requests = [];
      await run(recordingAdapter(requests), { timeoutSeconds });
      assert.equal(requests.length, 1);
      assert.equal(requests[0].timeout, expected);
    });
  }

  test(`${name} omits an unspecified timeout`, async () => {
    const requests = [];
    await run(recordingAdapter(requests));
    assert.equal(requests.length, 1);
    assert.equal(Object.hasOwn(requests[0], "timeout"), false);
  });

  for (const timeoutSeconds of [-0.000001, -0.001, -1, NaN, Infinity, -Infinity, Number.MAX_VALUE]) {
    test(`${name} rejects invalid timeout ${timeoutSeconds} before sending a request`, async () => {
      const requests = [];
      await assert.rejects(
        () => run(recordingAdapter(requests), { timeoutSeconds }),
        /timeoutSeconds/,
      );
      assert.equal(requests.length, 0);
    });
  }
}
