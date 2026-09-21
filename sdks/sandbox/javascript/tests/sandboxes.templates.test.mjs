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

import { SandboxesAdapter } from "../dist/internal.js";

function templateResponse(overrides = {}) {
  return {
    templateId: "tpl_123",
    image: "repo/img:tag",
    publish: "s3://bucket/publish",
    format: "overlaybd",
    status: { phase: "Pending" },
    createdAt: "2026-09-01T00:00:00Z",
    updatedAt: "2026-09-01T00:01:00Z",
    ...overrides,
  };
}

function listResponse(items, pagination = {}) {
  return {
    items,
    pagination: {
      page: 1,
      pageSize: 20,
      totalItems: items.length,
      totalPages: 1,
      hasNextPage: false,
      ...pagination,
    },
  };
}

test("createTemplate maps the create request body and parses timestamps", async () => {
  const requests = [];
  const adapter = new SandboxesAdapter({
    async POST(path, options) {
      assert.equal(path, "/templates");
      requests.push(options.body);
      return {
        data: templateResponse(),
        response: new Response(null, { status: 201 }),
      };
    },
  });

  const info = await adapter.createTemplate({
    image: "repo/img:tag",
    publish: "s3://bucket/publish",
    resourceLimits: { cpu: "2", memory: "1Gi", disk: "4Gi" },
    entrypoint: ["python", "app.py"],
    metadata: { team: "apollo" },
    readiness: { probe: "tcp://127.0.0.1:44772", warmupSeconds: 30 },
    format: "native",
  });

  assert.deepEqual(requests[0], {
    image: "repo/img:tag",
    publish: "s3://bucket/publish",
    resourceLimits: { cpu: "2", memory: "1Gi", disk: "4Gi" },
    entrypoint: ["python", "app.py"],
    metadata: { team: "apollo" },
    readiness: { probe: "tcp://127.0.0.1:44772", warmupSeconds: 30 },
    format: "native",
  });
  assert.equal(info.templateId, "tpl_123");
  assert.deepEqual(info.status, { phase: "Pending" });
  assert.ok(info.createdAt instanceof Date);
  assert.ok(info.updatedAt instanceof Date);
});

test("getTemplate returns the template with its build status", async () => {
  const paths = [];
  const adapter = new SandboxesAdapter({
    async GET(path, options) {
      paths.push([path, options.params.path]);
      return {
        data: templateResponse({
          status: { phase: "Succeeded", manifestRef: "s3://bucket/publish/manifest" },
        }),
        response: new Response(null, { status: 200 }),
      };
    },
  });

  const info = await adapter.getTemplate("tpl_123");

  assert.deepEqual(paths[0], ["/templates/{templateId}", { templateId: "tpl_123" }]);
  assert.equal(info.status.phase, "Succeeded");
  assert.equal(info.status.manifestRef, "s3://bucket/publish/manifest");
});

test("listTemplates forwards pagination and metadata filters", async () => {
  const queries = [];
  const adapter = new SandboxesAdapter({
    async GET(path, options) {
      assert.equal(path, "/templates");
      queries.push(options.params.query);
      return {
        data: listResponse([templateResponse()]),
        response: new Response(null, { status: 200 }),
      };
    },
  });

  const response = await adapter.listTemplates({
    metadata: { team: "apollo" },
    page: 2,
    pageSize: 10,
  });

  assert.deepEqual(queries[0], { metadata: "team=apollo", page: 2, pageSize: 10 });
  assert.equal(response.items.length, 1);
  assert.equal(response.items[0].templateId, "tpl_123");
  assert.ok(response.items[0].createdAt instanceof Date);
  assert.deepEqual(response.pagination, {
    page: 1,
    pageSize: 20,
    totalItems: 1,
    totalPages: 1,
    hasNextPage: false,
  });
});

test("metadata filters round-trip values containing &, = and %", async () => {
  const queries = [];
  const adapter = new SandboxesAdapter({
    async GET(_path, options) {
      queries.push(options.params.query);
      return {
        data: listResponse([], { totalItems: 0 }),
        response: new Response(null, { status: 200 }),
      };
    },
  });
  const metadata = { project: "Apollo", note: "a&b=c%d" };

  // Simulate the wire for both list surfaces: openapi-fetch encodes the joined
  // value once more, and the server decodes its layer before splitting with
  // parse_qsl.
  for (const list of [
    () => adapter.listTemplates({ metadata }),
    () => adapter.listSandboxes({ metadata }),
  ]) {
    queries.length = 0;
    await list();
    const raw = queries[0].metadata;
    const wire = encodeURIComponent(raw);
    const serverSide = decodeURIComponent(wire);
    assert.equal(serverSide, raw);
    const roundTripped = Object.fromEntries(new URLSearchParams(serverSide));
    assert.deepEqual(roundTripped, metadata);
  }
});

test("deleteTemplate issues DELETE against the template path", async () => {
  const paths = [];
  const adapter = new SandboxesAdapter({
    async DELETE(path, options) {
      paths.push([path, options.params.path]);
      return { response: new Response(null, { status: 204 }) };
    },
  });

  await adapter.deleteTemplate("tpl_123");

  assert.deepEqual(paths[0], ["/templates/{templateId}", { templateId: "tpl_123" }]);
});

test("createSandboxFromTemplate forwards only template-mode fields", async () => {
  const requests = [];
  const adapter = new SandboxesAdapter({
    async POST(path, options) {
      assert.equal(path, "/sandboxes");
      requests.push(options.body);
      return {
        data: {
          id: "sbx-tpl",
          status: { state: "Running" },
          entrypoint: ["tail", "-f", "/dev/null"],
          createdAt: "2026-09-01T00:00:00Z",
          expiresAt: null,
        },
        response: new Response(null, { status: 202 }),
      };
    },
  });

  const created = await adapter.createSandboxFromTemplate({
    templateId: "tpl_123",
    timeout: 300,
    metadata: { team: "apollo" },
    networkPolicy: { defaultAction: "deny", egress: [{ action: "allow", target: "pypi.org" }] },
    extensions: { "storage.id": "ext-1" },
  });

  assert.deepEqual(requests[0], {
    templateId: "tpl_123",
    timeout: 300,
    metadata: { team: "apollo" },
    networkPolicy: { defaultAction: "deny", egress: [{ action: "allow", target: "pypi.org" }] },
    extensions: { "storage.id": "ext-1" },
  });
  assert.equal(created.id, "sbx-tpl");
});

test("endpoints capture the OPEN-SANDBOX-ORIGIN response header", async () => {
  const adapter = new SandboxesAdapter({
    async GET(_path, _options) {
      return {
        data: { endpoint: "sandbox.internal:44772", headers: {} },
        response: new Response(null, {
          status: 200,
          headers: { "OPEN-SANDBOX-ORIGIN": "template" },
        }),
      };
    },
  });
  const unsignedAdapter = new SandboxesAdapter({
    async GET(_path, _options) {
      return {
        data: { endpoint: "sandbox.internal:44772", headers: {} },
        response: new Response(null, { status: 200 }),
      };
    },
  });

  const endpoint = await adapter.getSandboxEndpoint("sbx-1", 44772);
  const signed = await adapter.getSignedEndpoint("sbx-1", 44772, 1800000000);
  const unsigned = await unsignedAdapter.getSandboxEndpoint("sbx-1", 44772);

  assert.equal(endpoint.origin, "template");
  assert.equal(signed.origin, "template");
  assert.equal(unsigned.origin, undefined);
});
