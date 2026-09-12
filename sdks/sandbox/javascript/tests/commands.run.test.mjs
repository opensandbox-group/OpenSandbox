import assert from "node:assert/strict";
import test from "node:test";

import { CommandsAdapter, createExecdClient } from "../dist/internal.js";
import { SandboxApiException } from "../dist/index.js";

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

function createEarlyCloseStream() {
  // Delivers the two SSE chunks one read() at a time, then errors on the
  // next pull — simulating a peer that closes the connection right after
  // execution_complete, before the chunked terminator arrives (#1528).
  const encoder = new TextEncoder();
  const chunks = [
    'data: {"type":"init","text":"cmd-bg","timestamp":1}\n\n',
    'data: {"type":"execution_complete","timestamp":2,"execution_time":3}\n\n',
  ].map((chunk) => encoder.encode(chunk));
  let index = 0;
  return new ReadableStream({
    pull(controller) {
      if (index < chunks.length) {
        controller.enqueue(chunks[index]);
        index += 1;
        return;
      }
      controller.error(new Error("peer closed connection early"));
    },
  });
}

test("CommandsAdapter.run breaks on execution_complete for background commands", async () => {
  const fetchImpl = async () =>
    new Response(createEarlyCloseStream(), {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });

  const adapter = new CommandsAdapter(
    {},
    { baseUrl: "http://127.0.0.1:8080", fetch: fetchImpl },
  );

  const execution = await adapter.run("sleep 1", { background: true });

  assert.equal(execution.id, "cmd-bg");
  assert.equal(execution.complete?.executionTimeMs, 3);
  assert.equal(execution.exitCode, undefined);
});

test("CommandsAdapter.run still surfaces stream errors for foreground commands", async () => {
  const fetchImpl = async () =>
    new Response(createEarlyCloseStream(), {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });

  const adapter = new CommandsAdapter(
    {},
    { baseUrl: "http://127.0.0.1:8080", fetch: fetchImpl },
  );

  await assert.rejects(() => adapter.run("sleep 1"));
});

test("CommandsAdapter.run cancels the reader when a background command breaks early", async () => {
  // The stream never signals `done` or errors on its own past
  // execution_complete -- it simply stops delivering data, the way a
  // peer that never sends the chunked terminator would behave. If the
  // completion break left the reader un-cancelled, the stream's `cancel`
  // hook would never fire and the body would stay locked indefinitely.
  let cancelled = false;
  const encoder = new TextEncoder();
  const chunks = [
    'data: {"type":"init","text":"cmd-cancel","timestamp":1}\n\n',
    'data: {"type":"execution_complete","timestamp":2,"execution_time":3}\n\n',
  ].map((chunk) => encoder.encode(chunk));
  let index = 0;
  const stream = new ReadableStream({
    pull(controller) {
      if (index < chunks.length) {
        controller.enqueue(chunks[index]);
        index += 1;
      }
      // Beyond the known chunks: no-op. The underlying source never
      // closes or errors the stream by itself.
    },
    cancel() {
      cancelled = true;
    },
  });

  const fetchImpl = async () =>
    new Response(stream, {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });

  const adapter = new CommandsAdapter(
    {},
    { baseUrl: "http://127.0.0.1:8080", fetch: fetchImpl },
  );

  const execution = await adapter.run("sleep 1", { background: true });

  assert.equal(execution.id, "cmd-cancel");
  assert.equal(cancelled, true);
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

test("execd client error message carries unstructured JSON error body", async () => {
  const adapter = new CommandsAdapter(
    createExecdClient({
      baseUrl: "http://127.0.0.1:8080",
      fetch: async () =>
        new Response(JSON.stringify({ error: "invalid parameter" }), {
          status: 400,
          headers: { "content-type": "application/json" },
        }),
    }),
    {},
  );

  await assert.rejects(
    () => adapter.getCommandStatus("exec-1"),
    (err) => {
      assert.ok(err instanceof SandboxApiException);
      assert.match(err.message, /invalid parameter/);
      return true;
    },
  );
});

test("native argv preserves literals and execution options", async () => {
  const argv = ["tool", "", "a b", "$HOME", "x'y", "中文"];
  for (const background of [false, true]) {
    let body;
    const adapter = new CommandsAdapter({}, {
      baseUrl: "http://localhost",
      fetch: async (_url, init) => {
        body = JSON.parse(init.body);
        return new Response('data: {"type":"execution_complete"}\n\n', {headers: {"content-type": "text/event-stream"}});
      },
    });
    await adapter.run(argv, {background, workingDirectory: "$DIR", envs: {DIR: "/tmp"}, timeoutSeconds: 2});
    assert.deepEqual(body, {argv, background, cwd: "$DIR", envs: {DIR: "/tmp"}, timeout: 2000});
    await assert.rejects(adapter.run([]), /argv/);
    await assert.rejects(adapter.run(["tool", "\0"]), /argv/);
  }
});

test("native argv rejects invalid inputs before transport", async () => {
  const sparse = ["tool"];
  sparse.length = 2;
  let requests = 0;
  const adapter = new CommandsAdapter({}, {
    baseUrl: "http://localhost",
    fetch: async () => { requests++; throw new Error("unexpected request"); },
  });
  for (const input of [null, 123, {0: "tool", length: 1}, sparse, [], [""], ["tool", null], ["tool", "\0"]]) {
    await assert.rejects(adapter.run(input), /argv requires/);
    await assert.rejects(async () => {
      for await (const _ of adapter.runStream(input)) { /* consume */ }
    }, /argv requires/);
  }
  assert.equal(requests, 0);
});

for (const failure of [false, true]) {
  for (const lateOutput of [false, true]) {
    test(`foreground drains and releases stream: failure=${failure}, lateOutput=${lateOutput}`, async () => {
      const terminal = failure
        ? {type: "error", error: {ename: "CommandExecError", evalue: "7", traceback: []}}
        : {type: "execution_complete", execution_time: 1};
      const output = [{type: "stdout", text: "tail"}, {type: "stderr", text: "error-tail"}];
      const events = lateOutput ? [terminal, ...output] : [...output, terminal];
      const chunks = events.flatMap((event) => {
        const frame = new TextEncoder().encode(`data: ${JSON.stringify({...event, timestamp: 1})}\n\n`);
        return [frame.slice(0, 7), frame.slice(7)];
      });
      let exhausted = false;
      const stream = new ReadableStream({
        pull(controller) {
          if (chunks.length) controller.enqueue(chunks.shift());
          else { exhausted = true; controller.close(); }
        },
      });
      const execution = await createAdapter(stream).run("echo test");
      assert.deepEqual(execution.logs.stdout.map((item) => item.text), ["tail"]);
      assert.deepEqual(execution.logs.stderr.map((item) => item.text), ["error-tail"]);
      assert.equal(execution.exitCode, failure ? 7 : 0);
      assert.equal(exhausted, true);
      assert.equal(stream.locked, false);
    });
  }
}
