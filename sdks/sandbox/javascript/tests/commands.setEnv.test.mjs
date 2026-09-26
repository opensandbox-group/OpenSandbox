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

const SUCCESS_SSE = 'data: {"type":"execution_complete","timestamp":1,"execution_time":1}\n\n';

function createCaptureAdapter(sseBody = SUCCESS_SSE) {
  const requests = [];
  const fetchImpl = async (_url, init) => {
    requests.push(JSON.parse(init.body));
    return new Response(sseBody, {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });
  };
  const adapter = new CommandsAdapter({}, {
    baseUrl: "http://127.0.0.1:8080",
    fetch: fetchImpl,
  });
  return { adapter, requests };
}

function expectedSetEnvCommand(entry, key = "MY_TOKEN") {
  const quotedEntry = `'${entry.replaceAll("'", `'\\''`)}'`;
  return [
    `if [ -z "\${EXECD_ENVS:-}" ]; then printf '%s\\n' 'EXECD_ENVS is not set; cannot persist environment variable ${key}' >&2; exit 1; fi`,
    `mkdir -p "$(dirname "$EXECD_ENVS")"`,
    `printf '%s=%s\\n' ${quotedEntry} >> "$EXECD_ENVS"`,
  ].join("\n");
}

test("commands.setEnv appends the env entry via the sandbox env file", async () => {
  const { adapter, requests } = createCaptureAdapter();

  await adapter.setEnv("MY_TOKEN", "value");

  assert.equal(requests.length, 1);
  assert.deepEqual(requests[0], {
    command: expectedSetEnvCommand("MY_TOKEN='value'"),
    background: false,
  });
});

test("commands.setEnv keeps $, backslashes and newlines literal via the single-quoted form", async () => {
  const { adapter, requests } = createCaptureAdapter();

  const value = "line1\nline2 $HOME \\path";
  await adapter.setEnv("MY_VAR", value);

  assert.equal(requests[0].command, expectedSetEnvCommand("MY_VAR='line1\nline2 $HOME \\path'", "MY_VAR"));
});

test("commands.setEnv escapes single quotes via the double-quoted form", async () => {
  const { adapter, requests } = createCaptureAdapter();

  await adapter.setEnv("GREETING", "it's fine \"quoted\"\ttab");

  assert.equal(
    requests[0].command,
    expectedSetEnvCommand('GREETING="it\'s fine \\"quoted\\"\\ttab"', "GREETING"),
  );
});

test("commands.setEnv rejects invalid keys before any transport call", async () => {
  for (const key of ["", "1ABC", "MY-TOKEN", "MY TOKEN", "A=B", "A.B"]) {
    const { adapter, requests } = createCaptureAdapter();
    await assert.rejects(() => adapter.setEnv(key, "value"), /setEnv key must match/);
    assert.equal(requests.length, 0);
  }
});

test("commands.setEnv rejects NUL bytes in values before any transport call", async () => {
  const { adapter, requests } = createCaptureAdapter();

  await assert.rejects(() => adapter.setEnv("MY_TOKEN", "a\0b"), /NUL/);
  assert.equal(requests.length, 0);
});

test("commands.setEnv throws with stderr when the append command fails", async () => {
  const failureSse = [
    'data: {"type":"init","text":"cmd-1","timestamp":1}',
    'data: {"type":"stderr","text":"EXECD_ENVS is not set; cannot persist environment variable MY_TOKEN","timestamp":2}',
    'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"1","traceback":["exit status 1"]},"timestamp":3}',
    "",
  ].join("\n");
  const { adapter } = createCaptureAdapter(failureSse);

  await assert.rejects(
    () => adapter.setEnv("MY_TOKEN", "value"),
    (err) => {
      assert.match(err.message, /commands\.setEnv failed for 'MY_TOKEN'/);
      assert.match(err.message, /EXECD_ENVS is not set/);
      return true;
    },
  );
});
