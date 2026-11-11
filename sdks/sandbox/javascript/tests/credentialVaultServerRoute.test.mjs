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

import { createLifecycleClient, NetworkPolicyAdapter } from "../dist/internal.js";

/**
 * Wire tests for the server-routed Credential Vault adapter: verbs, paths,
 * and subresource name encoding must match the sidecar adapter's contract.
 */
function createCaptureClient(handler) {
  const requests = [];
  const fetchImpl = async (input, init) => {
    const isRequest = typeof input !== "string" && !(input instanceof URL);
    const url = new URL(isRequest ? input.url : String(input));
    let body = null;
    if (isRequest) {
      body = input.body === null ? null : await input.clone().text();
    } else {
      body = init?.body ?? null;
    }
    requests.push({
      method: isRequest ? input.method : (init?.method ?? "GET"),
      path: url.pathname,
      body,
    });
    const payload = handler(requests[requests.length - 1]);
    return new Response(payload.body ?? "", {
      status: payload.status ?? 200,
      headers: { "content-type": "application/json" },
    });
  };
  const client = createLifecycleClient({
    baseUrl: "http://127.0.0.1:8080/v1",
    apiKey: "test-key",
    fetch: fetchImpl,
  });
  return { adapter: new NetworkPolicyAdapter(client, "fsb-1"), requests };
}

test("create posts the vault body to the lifecycle path and parses the sanitized ack", async () => {
  const { adapter, requests } = createCaptureClient(() => ({
    status: 201,
    body: JSON.stringify({ revision: 1, credentials: [], bindings: [] }),
  }));

  const state = await adapter.create({ credentials: [], bindings: [] });

  assert.equal(requests[0].method, "POST");
  assert.equal(requests[0].path, "/v1/sandboxes/fsb-1/credential-vault");
  assert.deepEqual(state, { revision: 1, credentials: [], bindings: [] });
});

test("get and delete hit the bare vault path", async () => {
  const { adapter, requests } = createCaptureClient(() => ({
    status: 200,
    body: JSON.stringify({ revision: 2, credentials: [], bindings: [] }),
  }));

  await adapter.get();
  assert.deepEqual(requests[0], {
    method: "GET",
    path: "/v1/sandboxes/fsb-1/credential-vault",
    body: null,
  });

  await adapter.delete();
  assert.equal(requests[1].method, "DELETE");
  assert.equal(requests[1].path, "/v1/sandboxes/fsb-1/credential-vault");
});

test("subresource names are percent-encoded like the sidecar adapter", async () => {
  const metadata = JSON.stringify({
    name: "tok/one",
    sourceType: "inline",
    revision: 2,
  });
  const { adapter, requests } = createCaptureClient(() => ({
    status: 200,
    body: metadata,
  }));

  const credential = await adapter.getCredential("tok/one");

  assert.equal(
    requests[0].path,
    "/v1/sandboxes/fsb-1/credential-vault/credentials/tok%2Fone",
  );
  assert.equal(credential.name, "tok/one");
});

test("patch sends the mutation body and surfaces 412 with the server message", async () => {
  const { adapter, requests } = createCaptureClient((request) => {
    if (request.method === "PATCH") {
      return {
        status: 412,
        body: JSON.stringify({
          code: "FSB::API_ERROR",
          message: "credential vault revision conflict",
        }),
      };
    }
    return {
      status: 200,
      body: JSON.stringify({ revision: 2, credentials: [], bindings: [] }),
    };
  });

  await assert.rejects(
    () => adapter.patch({ expectedRevision: 2 }),
    (error) => {
      assert.equal(error.statusCode, 412);
      assert.match(error.message, /revision conflict/);
      return true;
    },
  );

  assert.equal(requests[0].method, "PATCH");
  assert.equal(requests[0].path, "/v1/sandboxes/fsb-1/credential-vault");
  assert.deepEqual(JSON.parse(requests[0].body), { expectedRevision: 2 });
});

test("getBinding hits the encoded bindings subresource and parses metadata", async () => {
  const { adapter, requests } = createCaptureClient(() => ({
    status: 200,
    body: JSON.stringify({
      name: "ext",
      revision: 2,
      match: { hosts: ["ext.test"] },
      auth: { type: "apiKey", name: "X-Api-Key" },
    }),
  }));

  const binding = await adapter.getBinding("ext/one");

  assert.equal(requests[0].method, "GET");
  assert.equal(
    requests[0].path,
    "/v1/sandboxes/fsb-1/credential-vault/bindings/ext%2Fone",
  );
  assert.equal(binding.name, "ext");
  assert.equal(binding.match.hosts[0], "ext.test");
  assert.equal(binding.auth.type, "apiKey");
});

test("list reads hit the credentials and bindings subresources", async () => {
  let call = 0;
  const { adapter, requests } = createCaptureClient(() => {
    call += 1;
    return {
      status: 200,
      body: JSON.stringify(
        call === 1
          ? { revision: 2, credentials: [] }
          : { revision: 2, bindings: [] },
      ),
    };
  });

  await adapter.listCredentials();
  await adapter.listBindings();

  assert.deepEqual(
    requests.map((r) => r.path),
    [
      "/v1/sandboxes/fsb-1/credential-vault/credentials",
      "/v1/sandboxes/fsb-1/credential-vault/bindings",
    ],
  );
});
