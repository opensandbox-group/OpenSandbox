import assert from "node:assert/strict";
import test from "node:test";

import { CodeInterpreter } from "../dist/index.js";
import { SandboxReadyTimeoutException } from "../../../sandbox/javascript/dist/index.js";

function fakeSandbox(commandResult) {
  const commands = {
    calls: [],
    async run(command) {
      commands.calls.push(command);
      if (typeof commandResult === "function") {
        return commandResult(commands.calls.length);
      }
      return commandResult ?? { error: null };
    },
  };
  return {
    sandbox: {
      id: "sandbox-id",
      connectionConfig: {
        protocol: "http",
      },
      commands,
      async getEndpoint(port) {
        return { endpoint: "sandbox.internal:3456", headers: {} };
      },
    },
    commands,
  };
}

test("CodeInterpreter.create passes the strict health check once the runtime port is listening", async () => {
  let pingAttempts = 0;
  const codes = {
    async ping() {
      pingAttempts += 1;
      return true;
    },
  };
  const adapterFactory = { createCodes: () => codes };
  const { sandbox, commands } = fakeSandbox((attempt) =>
    attempt >= 2 ? { error: null } : { error: { name: "CommandExecError" } },
  );

  const interpreter = await CodeInterpreter.create(sandbox, {
    adapterFactory,
    readyTimeoutSeconds: 5,
    healthCheckPollingInterval: 10,
  });

  assert.equal(pingAttempts, 2);
  assert.equal(commands.calls.length, 2);
  assert.ok(commands.calls[0].includes("/dev/tcp/127.0.0.1/"));
  assert.ok(commands.calls[0].includes("${JUPYTER_PORT:-44771}"));
  assert.equal(await interpreter.isHealthy(), true);
});

test("CodeInterpreter.create falls back to a direct execd ping when codes.ping is absent", async () => {
  const codes = {}; // custom adapter without the optional ping capability
  const adapterFactory = { createCodes: () => codes };
  const recorded = [];
  const base = fakeSandbox();
  const sandbox = {
    ...base.sandbox,
    connectionConfig: {
      protocol: "http",
      fetch: async (input) => {
        recorded.push(input instanceof Request ? input.url : String(input));
        return new Response("ok", { status: 200 });
      },
    },
  };

  const interpreter = await CodeInterpreter.create(sandbox, {
    adapterFactory,
    readyTimeoutSeconds: 5,
    healthCheckPollingInterval: 10,
  });

  assert.equal(await interpreter.isHealthy(), true);
  assert.equal(recorded.length >= 1, true);
  assert.match(recorded[0], /\/ping$/);
});

test("CodeInterpreter.create throws when execd never answers", async () => {
  const codes = {
    async ping() {
      return false;
    },
  };
  const adapterFactory = { createCodes: () => codes };
  const { sandbox, commands } = fakeSandbox();

  await assert.rejects(
    CodeInterpreter.create(sandbox, {
      adapterFactory,
      readyTimeoutSeconds: 0.05,
      healthCheckPollingInterval: 10,
    }),
    SandboxReadyTimeoutException,
  );
  // The runtime-process leg must not run when the daemon ping fails.
  assert.equal(commands.calls.length, 0);
});

test("CodeInterpreter.create throws when the runtime port never listens", async () => {
  const codes = {
    async ping() {
      return true;
    },
  };
  const adapterFactory = { createCodes: () => codes };
  const { sandbox } = fakeSandbox({ error: { name: "CommandExecError" } });

  await assert.rejects(
    CodeInterpreter.create(sandbox, {
      adapterFactory,
      readyTimeoutSeconds: 0.05,
      healthCheckPollingInterval: 10,
    }),
    SandboxReadyTimeoutException,
  );
});

test("CodeInterpreter.create treats ping errors as unhealthy and keeps polling", async () => {
  let attempts = 0;
  const codes = {
    async ping() {
      attempts += 1;
      if (attempts < 2) {
        throw new Error("connection refused");
      }
      return true;
    },
  };
  const adapterFactory = { createCodes: () => codes };
  const { sandbox } = fakeSandbox();

  await CodeInterpreter.create(sandbox, {
    adapterFactory,
    readyTimeoutSeconds: 5,
    healthCheckPollingInterval: 10,
  });

  assert.equal(attempts, 2);
});

test("CodeInterpreter.create skips the health check when skipHealthCheck is set", async () => {
  const codes = {
    async ping() {
      throw new Error("ping must not be called when skipHealthCheck is set");
    },
  };
  const adapterFactory = { createCodes: () => codes };
  const { sandbox, commands } = fakeSandbox({ error: null });

  const interpreter = await CodeInterpreter.create(sandbox, {
    adapterFactory,
    skipHealthCheck: true,
  });

  assert.equal(interpreter.id, "sandbox-id");
  assert.equal(commands.calls.length, 0);
});
