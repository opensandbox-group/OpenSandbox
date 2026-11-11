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

// Regression tests for the #1768 SDK audit fixes on the code interpreter:
// - run() accepts a matching context+language pair (only mismatches throw)
// - interrupt() targets an execution id, named accordingly
// - the core run() streaming path is covered for both NDJSON and
//   SSE `data:`-prefixed frames (loop and tail-flush)

import assert from "node:assert/strict";
import test from "node:test";

import { DefaultAdapterFactory, SupportedLanguages } from "../dist/index.js";
import { InvalidArgumentException } from "@alibaba-group/opensandbox";

function sseResponse(frames, { contentType = "text/event-stream" } = {}) {
  const encoder = new TextEncoder();
  const stream = new ReadableStream({
    start(controller) {
      for (const frame of frames) {
        controller.enqueue(encoder.encode(frame));
      }
      controller.close();
    },
  });
  return new Response(stream, {
    status: 200,
    headers: { "content-type": contentType },
  });
}

function makeCodes({ fetchImpl, sseFetchImpl }) {
  const factory = new DefaultAdapterFactory();
  return factory.createCodes({
    sandbox: {
      connectionConfig: {
        headers: {},
        fetch: fetchImpl,
        sseFetch: sseFetchImpl,
      },
    },
    execdBaseUrl: "http://sandbox.internal:3456",
    endpointHeaders: {},
  });
}

test("run() accepts a context and matching language, rejecting only mismatches", async () => {
  const bodies = [];
  const fetchImpl = async () => new Response("unused", { status: 404 });
  const sseFetchImpl = async (input, init = {}) => {
    bodies.push(JSON.parse(String(init.body)));
    return sseResponse([
      `{"type":"stdout","timestamp":1,"text":"hi"}\n`,
      `{"type":"execution_complete","timestamp":2,"execution_time":3}\n`,
    ]);
  };
  const codes = makeCodes({ fetchImpl, sseFetchImpl });

  // Regression: this used to throw "Provide either opts.context or
  // opts.language, not both" even when the pair matched (Python accepts it).
  const execution = await codes.run("print('hi')", {
    context: { id: "ctx-1", language: SupportedLanguages.PYTHON },
    language: SupportedLanguages.PYTHON,
  });
  assert.equal(execution.logs.stdout[0].text, "hi");
  assert.equal(bodies[0].context.id, "ctx-1");
  assert.equal(bodies[0].context.language, "python");

  await assert.rejects(
    codes.run("print('hi')", {
      context: { id: "ctx-1", language: SupportedLanguages.PYTHON },
      language: SupportedLanguages.GO,
    }),
    (err) => {
      assert.ok(err instanceof InvalidArgumentException);
      assert.match(err.message, /must match context\.language/);
      return true;
    },
  );
});

test("interrupt() sends the execution id as the id query parameter", async () => {
  const recorded = [];
  const fetchImpl = async (input, init = {}) => {
    const request = input instanceof Request ? input : new Request(input, init);
    recorded.push(`${request.method} ${request.url}`);
    return new Response(null, { status: 204 });
  };
  const codes = makeCodes({ fetchImpl, sseFetchImpl: fetchImpl });

  await codes.interrupt("exec-42");
  assert.deepEqual(recorded, ["DELETE http://sandbox.internal:3456/code?id=exec-42"]);

  // The parameter is an execution id; an empty value must fail fast.
  await assert.rejects(codes.interrupt(""), InvalidArgumentException);
});

test("run() parses SSE data:-framed events, including an unterminated tail frame", async () => {
  const sseFetchImpl = async () =>
    sseResponse([
      // Framed events with blank-line separators...
      `data: {"type":"stdout","timestamp":1,"text":"a"}\n\n`,
      `data: {"type":"stdout","timestamp":2,"text":"b"}\n\n`,
      // ...and a tail frame flushed without a trailing blank line.
      `data: {"type":"execution_complete","timestamp":3,"execution_time":4}`,
    ]);
  const codes = makeCodes({ fetchImpl: async () => new Response("unused", { status: 404 }), sseFetchImpl });

  const execution = await codes.run("print('ab')");
  assert.deepEqual(
    execution.logs.stdout.map((entry) => entry.text),
    ["a", "b"],
  );
});
