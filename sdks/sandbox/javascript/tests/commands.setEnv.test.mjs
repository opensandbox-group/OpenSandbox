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
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync } from "node:fs";
import test from "node:test";
import { tmpdir } from "node:os";
import { join } from "node:path";

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

// Round-trip cases: [key, value, the exact line the snippet must append to the
// env file]. The expected lines are hand-written literals.
const ROUND_TRIP_CASES = [
  ["MY_TOKEN", "value", "MY_TOKEN='value'"],
  ["MY_VAR", "line1\nline2 $HOME \\path", "MY_VAR='line1\nline2 $HOME \\path'"],
  ["KV", "a=b=c", "KV='a=b=c'"],
  ["EMPTY", "", "EMPTY=''"],
  ["GREETING", "it's fine \"quoted\"\ttab", 'GREETING="it\'s fine \\"quoted\\"\\ttab"'],
  ["PATHY", "it's\nC:\\path", 'PATHY="it\'s\\nC:\\\\path"'],
];

test("commands.setEnv appends the env entry via the sandbox env file", async () => {
  const { adapter, requests } = createCaptureAdapter();

  await adapter.setEnv("MY_TOKEN", "value");

  assert.equal(requests.length, 1);
  // Hand-written golden literal (not derived from the implementation).
  assert.equal(
    requests[0].command,
    'if [ -z "${EXECD_ENVS:-}" ]; then printf \'%s\\n\' \'EXECD_ENVS is not set; cannot persist environment variable MY_TOKEN\' >&2; exit 1; fi\n'
    + 'mkdir -p "$(dirname "$EXECD_ENVS")"\n'
    + `printf '%s\\n' 'MY_TOKEN='\\''value'\\''' >> "$EXECD_ENVS"`,
  );
  assert.deepEqual(requests[0], { command: requests[0].command, background: false });
});

test("commands.setEnv snippet appends well-formed lines round-tripped through sh", async () => {
  const { adapter, requests } = createCaptureAdapter();

  for (const [key, value] of ROUND_TRIP_CASES) {
    await adapter.setEnv(key, value);
    const command = requests.at(-1).command;
    const envFile = join(mkdtempSync(join(tmpdir(), "opensandbox-setenv-")), ".env");
    execFileSync("/bin/sh", ["-c", command], {
      env: { PATH: process.env.PATH, EXECD_ENVS: envFile },
    });
    const want = ROUND_TRIP_CASES.find(([k]) => k === key)[2] + "\n";
    assert.equal(readFileSync(envFile, "utf8"), want, `round-trip failed for ${key}`);
  }
});

test("commands.setEnv rejects invalid keys before any transport call", async () => {
  for (const key of ["", "1ABC", "MY-TOKEN", "MY TOKEN", "A=B", "A.B", "A\n"]) {
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

test("commands.setEnv treats a dropped stream without completion as failure", async () => {
  // Stream ends after init only: no execution_complete and no error event, so
  // the append was never confirmed and setEnv must not report success.
  const incompleteSse = 'data: {"type":"init","text":"cmd-1","timestamp":1}\n\n';
  const { adapter } = createCaptureAdapter(incompleteSse);

  await assert.rejects(() => adapter.setEnv("MY_TOKEN", "value"), /commands\.setEnv failed/);
});
