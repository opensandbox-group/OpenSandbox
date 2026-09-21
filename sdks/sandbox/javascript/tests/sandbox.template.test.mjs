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

import {
  ConnectionConfig,
  DEFAULT_EXECD_PORT,
  Sandbox,
  SandboxOrigin,
} from "../dist/index.js";

function createTemplateFactory({ includeNetworkPolicyStack = true, execdOrigin } = {}) {
  const calls = [];
  const createdRequests = [];
  const policyOps = [];
  const networkPolicyEgress = {
    async getPolicy() {
      policyOps.push("getPolicy");
      return {
        defaultAction: "deny",
        egress: [{ action: "allow", target: "pypi.org" }],
      };
    },
    async patchRules(rules) {
      policyOps.push(["patchRules", rules]);
    },
    async deleteRules(targets) {
      policyOps.push(["deleteRules", targets]);
    },
  };
  const sandboxes = {
    async createSandbox(req) {
      createdRequests.push(req);
      return {
        id: "sbx-created",
        status: { state: "Running" },
        entrypoint: [],
        createdAt: "2026-09-01T00:00:00Z",
        expiresAt: null,
      };
    },
    async createSandboxFromTemplate(req) {
      createdRequests.push(req);
      return {
        id: "sbx-tpl",
        status: { state: "Running" },
        entrypoint: [],
        createdAt: "2026-09-01T00:00:00Z",
        expiresAt: null,
      };
    },
    async getSandbox() {
      throw new Error("not implemented");
    },
    async listSandboxes() {
      throw new Error("not implemented");
    },
    async deleteSandbox() {},
    async pauseSandbox() {},
    async resumeSandbox() {},
    async getSandboxEndpoint(_sandboxId, port) {
      calls.push(["getSandboxEndpoint", port]);
      const endpoint = { endpoint: `127.0.0.1:${port}`, headers: {} };
      if (execdOrigin != null && port === DEFAULT_EXECD_PORT) {
        endpoint.origin = execdOrigin;
      }
      return endpoint;
    },
  };
  const adapterFactory = {
    createLifecycleStack() {
      return { sandboxes };
    },
    createExecdStack() {
      calls.push(["createExecdStack"]);
      return { commands: {}, files: {}, health: {}, metrics: {} };
    },
    createEgressStack() {
      calls.push(["createEgressStack"]);
      return {
        egress: {
          async getPolicy() {
            return { defaultAction: "deny", egress: [] };
          },
          async patchRules() {},
          async deleteRules() {},
        },
      };
    },
  };
  if (includeNetworkPolicyStack) {
    adapterFactory.createNetworkPolicyStack = (opts) => {
      calls.push(["createNetworkPolicyStack", opts.sandboxId]);
      return { egress: networkPolicyEgress };
    };
  }
  return { adapterFactory, calls, createdRequests, policyOps };
}

test("Sandbox.createFromTemplate creates via the template request and routes egress through the control plane", async () => {
  const { adapterFactory, calls, createdRequests, policyOps } = createTemplateFactory();
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const sandbox = await Sandbox.createFromTemplate({
    adapterFactory,
    connectionConfig,
    templateId: "tpl_123",
    timeoutSeconds: 300,
    metadata: { team: "apollo" },
    networkPolicy: { egress: [{ action: "allow", target: "pypi.org" }] },
    extensions: { "storage.id": "ext-1" },
    skipHealthCheck: true,
  });

  assert.equal(sandbox.id, "sbx-tpl");
  assert.equal(sandbox.origin, SandboxOrigin.TEMPLATE);
  assert.deepEqual(createdRequests, [
    {
      templateId: "tpl_123",
      timeout: 300,
      metadata: { team: "apollo" },
      networkPolicy: {
        defaultAction: "deny",
        egress: [{ action: "allow", target: "pypi.org" }],
      },
      extensions: { "storage.id": "ext-1" },
    },
  ]);
  // Template-backed sandboxes have no egress sidecar endpoint.
  assert.deepEqual(
    calls.filter(([method]) => method === "getSandboxEndpoint").map(([, port]) => port),
    [DEFAULT_EXECD_PORT],
  );
  assert.equal(calls.filter(([method]) => method === "createEgressStack").length, 0);
  assert.deepEqual(
    calls.find(([method]) => method === "createNetworkPolicyStack"),
    ["createNetworkPolicyStack", "sbx-tpl"],
  );

  await sandbox.patchEgressRules([{ action: "allow", target: "www.github.com" }]);
  assert.deepEqual(await sandbox.getEgressPolicy(), {
    defaultAction: "deny",
    egress: [{ action: "allow", target: "pypi.org" }],
  });
  assert.deepEqual(policyOps, [
    ["patchRules", [{ action: "allow", target: "www.github.com" }]],
    "getPolicy",
  ]);
});

test("Sandbox.createFromTemplate has no Credential Vault", async () => {
  const { adapterFactory } = createTemplateFactory();
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const sandbox = await Sandbox.createFromTemplate({
    adapterFactory,
    connectionConfig,
    templateId: "tpl_123",
    timeoutSeconds: 300,
    skipHealthCheck: true,
  });

  await assert.rejects(
    () => sandbox.credentialVault.get(),
    /Credential Vault is not available for template-backed sandboxes/,
  );
});

test("Sandbox.createFromTemplate requires a template id", async () => {
  const { adapterFactory } = createTemplateFactory();
  for (const templateId of ["", "   "]) {
    await assert.rejects(
      Sandbox.createFromTemplate({
        adapterFactory,
        connectionConfig: { domain: "http://127.0.0.1:8080" },
        templateId,
        timeoutSeconds: 300,
        skipHealthCheck: true,
      }),
      /Template ID must be specified/,
    );
  }
});

test("Sandbox.createFromTemplate requires a finite timeoutSeconds", async () => {
  const { adapterFactory } = createTemplateFactory();
  for (const timeoutSeconds of [undefined, Number.NaN, Number.POSITIVE_INFINITY]) {
    await assert.rejects(
      Sandbox.createFromTemplate({
        adapterFactory,
        connectionConfig: { domain: "http://127.0.0.1:8080" },
        templateId: "tpl_123",
        timeoutSeconds,
        skipHealthCheck: true,
      }),
      /timeoutSeconds must be a finite number/,
    );
  }
});

test("Sandbox.createFromTemplate requires a template-aware adapter factory", async () => {
  const { adapterFactory } = createTemplateFactory({ includeNetworkPolicyStack: false });

  await assert.rejects(
    Sandbox.createFromTemplate({
      adapterFactory,
      connectionConfig: { domain: "http://127.0.0.1:8080" },
      templateId: "tpl_123",
      timeoutSeconds: 300,
      skipHealthCheck: true,
    }),
    /does not provide createNetworkPolicyStack/,
  );
});

test("Sandbox.create detects template-backed sandboxes from the endpoint origin header", async () => {
  const { adapterFactory, calls } = createTemplateFactory({ execdOrigin: "template" });
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const sandbox = await Sandbox.create({
    adapterFactory,
    connectionConfig,
    image: "python:3.12",
    timeoutSeconds: null,
    skipHealthCheck: true,
  });

  assert.equal(sandbox.id, "sbx-created");
  assert.equal(sandbox.origin, SandboxOrigin.TEMPLATE);
  assert.deepEqual(
    calls.filter(([method]) => method === "getSandboxEndpoint").map(([, port]) => port),
    [DEFAULT_EXECD_PORT],
  );
  assert.equal(calls.filter(([method]) => method === "createEgressStack").length, 0);
  await assert.rejects(() => sandbox.credentialVault.get(), /template-backed/);
});

test("Sandbox.connect detects template-backed sandboxes from the endpoint origin header", async () => {
  const { adapterFactory, calls } = createTemplateFactory({ execdOrigin: "template" });
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const sandbox = await Sandbox.connect({
    adapterFactory,
    connectionConfig,
    sandboxId: "sbx-tpl",
    skipHealthCheck: true,
  });

  assert.equal(sandbox.origin, SandboxOrigin.TEMPLATE);
  assert.deepEqual(
    calls.filter(([method]) => method === "getSandboxEndpoint").map(([, port]) => port),
    [DEFAULT_EXECD_PORT],
  );
  assert.equal(calls.filter(([method]) => method === "createEgressStack").length, 0);
  assert.deepEqual(await sandbox.getEgressPolicy(), {
    defaultAction: "deny",
    egress: [{ action: "allow", target: "pypi.org" }],
  });
});

test("Sandbox.connect keeps the sidecar egress for non-template sandboxes", async () => {
  const { adapterFactory, calls } = createTemplateFactory();
  const connectionConfig = new ConnectionConfig({ domain: "http://127.0.0.1:8080" });
  connectionConfig.withTransportIfMissing = () => connectionConfig;

  const sandbox = await Sandbox.connect({
    adapterFactory,
    connectionConfig,
    sandboxId: "sbx-1",
    skipHealthCheck: true,
  });

  assert.equal(sandbox.origin, SandboxOrigin.UNKNOWN);
  assert.ok(
    calls.some(([method]) => method === "createEgressStack"),
    "expected the sidecar egress stack to be created",
  );
});
