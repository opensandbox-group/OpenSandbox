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

import { CodeInterpreter } from "../dist/index.js";
import { DEFAULT_EXECD_PORT } from "../../../sandbox/javascript/dist/index.js";

test("CodeInterpreter.create forwards endpoint headers to adapter factory", async () => {
  const calls = [];
  const sandbox = {
    connectionConfig: {
      protocol: "https",
      headers: { "x-global": "global" },
    },
    commands: {
      async run() {
        return { error: null };
      },
    },
    async getEndpoint(port) {
      assert.equal(port, DEFAULT_EXECD_PORT);
      return {
        endpoint: "sandbox.internal:3456",
        headers: { "x-endpoint": "endpoint" },
      };
    },
  };
  const codes = {
    kind: "codes",
    async ping() {
      return true;
    },
  };
  const adapterFactory = {
    createCodes(opts) {
      calls.push(opts);
      return codes;
    },
  };

  const interpreter = await CodeInterpreter.create(sandbox, { adapterFactory });

  assert.equal(interpreter.codes, codes);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].execdBaseUrl, "https://sandbox.internal:3456");
  assert.deepEqual(calls[0].endpointHeaders, { "x-endpoint": "endpoint" });
});
