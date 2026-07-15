import assert from "node:assert/strict";
import test from "node:test";

import {
  ConnectionConfig,
  DEFAULT_EGRESS_PORT,
  DEFAULT_EXECD_PORT,
  Sandbox,
} from "../dist/index.js";

function createAdapterFactory() {
  const calls = [];
  const sandboxes = {
    async getSandboxEndpoint(sandboxId, port, useServerProxy) {
      calls.push({ method: "getSandboxEndpoint", args: [sandboxId, port, useServerProxy] });
      return {
        endpoint: `sandbox.internal:${port}`,
        headers: { "x-port": String(port) },
      };
    },
    async resumeSandbox(sandboxId) {
      calls.push({ method: "resumeSandbox", args: [sandboxId] });
    },
    async getSandbox() {
      throw new Error("not implemented");
    },
    async listSandboxes() {
      throw new Error("not implemented");
    },
    async createSandbox() {
      throw new Error("not implemented");
    },
    async deleteSandbox() {},
    async pauseSandbox() {},
    async renewSandboxExpiration() {
      throw new Error("not implemented");
    },
  };

  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack(opts) {
      calls.push({ method: "createExecdStack", args: [opts] });
      return {
        commands: { kind: "commands" },
        files: { kind: "files" },
        health: { async ping() { return true; } },
        metrics: { kind: "metrics" },
      };
    },
    createEgressStack(opts) {
      calls.push({ method: "createEgressStack", args: [opts] });
      return {
        egress: {
          async getPolicy() {
            return { defaultAction: "deny", egress: [] };
          },
          async patchRules() {},
        },
      };
    },
  };

  return { adapterFactory, calls };
}

test("Sandbox.connect wires execd and egress stacks and getEndpointUrl uses protocol", async () => {
  const { adapterFactory, calls } = createAdapterFactory();
  const connectionConfig = new ConnectionConfig({ domain: "https://api.opensandbox.test" });
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const sandbox = await Sandbox.connect({
    sandboxId: "sbx-1",
    connectionConfig,
    adapterFactory,
    skipHealthCheck: true,
  });

  assert.equal(sandbox.id, "sbx-1");
  assert.equal(await sandbox.getEndpointUrl(8080), "https://sandbox.internal:8080");
  assert.deepEqual(
    calls
      .filter((entry) => entry.method === "getSandboxEndpoint")
      .map((entry) => entry.args.slice(0, 2)),
    [
      ["sbx-1", DEFAULT_EXECD_PORT],
      ["sbx-1", DEFAULT_EGRESS_PORT],
      ["sbx-1", 8080],
    ],
  );
  assert.equal(calls[2].method, "createExecdStack");
  assert.equal(calls[2].args[0].execdBaseUrl, `https://sandbox.internal:${DEFAULT_EXECD_PORT}`);
  assert.deepEqual(calls[2].args[0].endpointHeaders, { "x-port": String(DEFAULT_EXECD_PORT) });
});

test("Sandbox.resume refreshes endpoints through connect after resuming lifecycle", async () => {
  const { adapterFactory, calls } = createAdapterFactory();
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const sandbox = await Sandbox.connect({
    sandboxId: "sbx-2",
    connectionConfig,
    adapterFactory,
    skipHealthCheck: true,
  });

  calls.length = 0;
  const resumed = await sandbox.resume({ skipHealthCheck: true });

  assert.equal(resumed.id, "sbx-2");
  assert.equal(calls[0].method, "resumeSandbox");
  assert.deepEqual(calls[0].args, ["sbx-2"]);
  assert.deepEqual(
    calls
      .filter((entry) => entry.method === "getSandboxEndpoint")
      .map((entry) => entry.args.slice(0, 2)),
    [
      ["sbx-2", DEFAULT_EXECD_PORT],
      ["sbx-2", DEFAULT_EGRESS_PORT],
    ],
  );
});
