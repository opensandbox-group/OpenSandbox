import assert from "node:assert/strict";
import test from "node:test";

import { CommandsAdapter } from "../dist/internal.js";

function createAdapter(responseBody, opts = {}) {
  const fetchImpl = async () =>
    new Response(responseBody, {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });

  return new CommandsAdapter(
    {},
    {
      baseUrl: "http://127.0.0.1:8080",
      fetch: fetchImpl,
      headers: opts.headers,
    },
  );
}

test("CommandsAdapter.run populates complete and exitCode for successful foreground commands", async () => {
  const adapter = createAdapter(
    [
      'data: {"type":"init","text":"cmd-1","timestamp":1}',
      'data: {"type":"stdout","text":"hi","timestamp":2}',
      'data: {"type":"execution_complete","timestamp":3,"execution_time":4}',
      "",
    ].join("\n"),
  );

  const execution = await adapter.run("echo hi");

  assert.equal(execution.id, "cmd-1");
  assert.equal(execution.logs.stdout[0].text, "hi");
  assert.equal(execution.complete?.executionTimeMs, 4);
  assert.equal(execution.exitCode, 0);
});

test("CommandsAdapter.run infers non-zero exitCode from final error state", async () => {
  const adapter = createAdapter(
    [
      'data: {"type":"init","text":"cmd-2","timestamp":1}',
      'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"7","traceback":["exit status 7"]},"timestamp":2}',
      "",
    ].join("\n"),
  );

  const execution = await adapter.run("exit 7");

  assert.equal(execution.id, "cmd-2");
  assert.equal(execution.error?.value, "7");
  assert.equal(execution.complete, undefined);
  assert.equal(execution.exitCode, 7);
});

test("CommandsAdapter.run keeps exitCode null when error value is empty", async () => {
  const adapter = createAdapter(
    [
      'data: {"type":"init","text":"cmd-3","timestamp":1}',
      'data: {"type":"execution_complete","timestamp":2,"execution_time":4}',
      'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"","traceback":["failed"]},"timestamp":3}',
      "",
    ].join("\n"),
  );

  const execution = await adapter.run("bad command");

  assert.equal(execution.id, "cmd-3");
  assert.equal(execution.error?.value, "");
  assert.equal(execution.complete?.executionTimeMs, 4);
  assert.equal(execution.exitCode, null);
});

test("CommandsAdapter.runInSession sends command and timeout fields", async () => {
  let requestBody;
  const fetchImpl = async (url, init) => {
    requestBody = JSON.parse(init.body);
    assert.equal(url, "http://127.0.0.1:8080/session/sess-1/run");
    return new Response(
      [
        'data: {"type":"stdout","text":"ok","timestamp":1}',
        'data: {"type":"execution_complete","timestamp":2,"execution_time":3}',
        "",
      ].join("\n"),
      {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      },
    );
  };

  const adapter = new CommandsAdapter(
    {},
    {
      baseUrl: "http://127.0.0.1:8080",
      fetch: fetchImpl,
    },
  );

  const execution = await adapter.runInSession("sess-1", "pwd", {
    workingDirectory: "/var",
    timeoutSeconds: 5,
  });

  assert.deepEqual(requestBody, {
    command: "pwd",
    cwd: "/var",
    timeout: 5000,
  });
  assert.equal(execution.logs.stdout[0].text, "ok");
  assert.equal(execution.exitCode, 0);
});

test("CommandsAdapter.run auto-resumes on SSE disconnect", async () => {
  const initialEvents = [
    { type: "init", text: "cmd-resume", timestamp: 1, eid: 1 },
    { type: "stdout", text: "before-disconnect", timestamp: 2, eid: 2 },
  ];
  const resumeEvents = [
    { type: "stdout", text: "after-resume", timestamp: 3, eid: 3 },
    { type: "execution_complete", timestamp: 4, execution_time: 10, eid: 4 },
  ];

  let callCount = 0;
  const fetchImpl = async (url, init) => {
    callCount++;
    if (callCount === 1) {
      const body = initialEvents.map(e => `data: ${JSON.stringify(e)}`).join("\n") + "\n";
      const stream = new ReadableStream({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(body));
        },
        pull(controller) {
          controller.error(new TypeError("fetch failed"));
        },
      });
      return new Response(stream, {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      });
    }
    // Resume call
    assert.ok(url.includes("/resume?after_eid=2"), `unexpected resume URL: ${url}`);
    const body = resumeEvents.map(e => `data: ${JSON.stringify(e)}`).join("\n") + "\n\n";
    return new Response(body, {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });
  };

  const adapter = new CommandsAdapter(
    {},
    {
      baseUrl: "http://127.0.0.1:8080",
      fetch: fetchImpl,
    },
  );

  const execution = await adapter.run("echo test");

  assert.equal(execution.id, "cmd-resume");
  assert.equal(execution.logs.stdout.length, 2);
  assert.equal(execution.logs.stdout[0].text, "before-disconnect");
  assert.equal(execution.logs.stdout[1].text, "after-resume");
  assert.equal(execution.lastEid, 4);
  assert.equal(execution.exitCode, 0);
  assert.equal(callCount, 2);
});

test("CommandsAdapter.runInSession infers non-zero exitCode from final error state", async () => {
  const adapter = createAdapter(
    [
      'data: {"type":"init","text":"sess-cmd-2","timestamp":1}',
      'data: {"type":"error","error":{"ename":"CommandExecError","evalue":"7","traceback":["exit status 7"]},"timestamp":2}',
      "",
    ].join("\n"),
  );

  const execution = await adapter.runInSession("sess-2", "exit 7");

  assert.equal(execution.id, "sess-cmd-2");
  assert.equal(execution.error?.value, "7");
  assert.equal(execution.complete, undefined);
  assert.equal(execution.exitCode, 7);
});
