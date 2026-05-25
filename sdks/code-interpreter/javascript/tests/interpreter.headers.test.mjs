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
    async getEndpoint(port) {
      assert.equal(port, DEFAULT_EXECD_PORT);
      return {
        endpoint: "sandbox.internal:3456",
        headers: { "x-endpoint": "endpoint" },
      };
    },
  };
  const codes = { kind: "codes" };
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
