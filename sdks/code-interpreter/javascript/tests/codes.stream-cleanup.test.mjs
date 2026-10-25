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

function createCodes(body) {
  return new DefaultAdapterFactory().createCodes({
    sandbox: {
      connectionConfig: {
        headers: {},
        sseFetch: async () => new Response(body, {
          headers: { "content-type": "text/event-stream" },
        }),
      },
    },
    execdBaseUrl: "http://sandbox.invalid:44772",
    endpointHeaders: {},
  });
}

function encodeEvent(event) {
  return new TextEncoder().encode(`data: ${JSON.stringify(event)}\n\n`);
}

const stdout = { type: "stdout", text: "hello", timestamp: 1 };

test("runStream cancels an unfinished response when the consumer breaks", async () => {
  let cancellations = 0;
  const body = new ReadableStream({
    start(controller) {
      controller.enqueue(encodeEvent(stdout));
      // Keep the source open after the first event.
    },
    cancel() {
      cancellations += 1;
    },
  });

  const events = [];
  for await (const event of createCodes(body).runStream({ code: "print('hello')", context: { language: "python" } })) {
    events.push(event);
    break;
  }

  assert.deepEqual(events, [stdout]);
  assert.equal(cancellations, 1);
  assert.equal(body.locked, false);
});

for (const rejectCancellation of [false, true]) {
  test(`run cleans up after a handler rejects (cancel rejects: ${rejectCancellation})`, async () => {
    const handlerError = new Error("handler failed");
    let cancellations = 0;
    const body = new ReadableStream({
      start(controller) {
        controller.enqueue(encodeEvent(stdout));
      },
      cancel() {
        cancellations += 1;
        if (rejectCancellation) return Promise.reject(new Error("cleanup failed"));
      },
    });

    await assert.rejects(createCodes(body).run("print('hello')", {
      handlers: { onStdout: async () => { throw handlerError; } },
    }), (error) => error === handlerError);

    assert.equal(cancellations, 1);
    assert.equal(body.locked, false);
  });
}

test("run releases the reader and preserves a response read error", async () => {
  const readError = new Error("connection lost");
  const body = new ReadableStream({
    start(controller) {
      controller.enqueue(encodeEvent(stdout));
    },
    pull(controller) {
      controller.error(readError);
    },
  });

  await assert.rejects(createCodes(body).run("print('hello')"), (error) => error === readError);
  assert.equal(body.locked, false);
});

test("run releases the reader after draining output and the final event", async () => {
  const body = new ReadableStream({
    start(controller) {
      controller.enqueue(encodeEvent(stdout));
      // Preserve the existing support for a final event without a newline.
      controller.enqueue(new TextEncoder().encode('data: {"type":"execution_complete","timestamp":2,"execution_time":3}'));
      controller.close();
    },
  });

  const execution = await createCodes(body).run("print('hello')");

  assert.deepEqual(execution.logs.stdout.map((entry) => entry.text), ["hello"]);
  assert.equal(execution.complete.executionTimeMs, 3);
  assert.equal(body.locked, false);
});
