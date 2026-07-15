import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import test from "node:test";

import { EgressAdapter } from "../dist/internal.js";

const credential = {
  name: "api-token",
  source: { value: "write-only-value" },
};

const binding = {
  name: "api-binding",
  match: {
    schemes: ["https"],
    hosts: ["api.example.com"],
    methods: ["GET", "POST"],
    paths: ["/v1/*"],
  },
  auth: {
    type: "apiKey",
    name: "X-API-Key",
    credential: "api-token",
  },
};

const sanitizedCredential = {
  name: "api-token",
  sourceType: "inline",
  revision: 7,
};

const sanitizedBinding = {
  name: "api-binding",
  revision: 5,
  match: {
    schemes: ["https"],
    hosts: ["api.example.com"],
    methods: ["GET", "POST"],
    paths: ["/v1/*"],
  },
  auth: {
    type: "apiKey",
    name: "X-API-Key",
  },
};

function stateResponse() {
  return {
    revision: 9,
    credentials: [
      {
        ...sanitizedCredential,
        source: { type: "inline", value: "write-only-value" },
      },
    ],
    bindings: [
      {
        ...sanitizedBinding,
        auth: {
          ...sanitizedBinding.auth,
          credential: "api-token",
          headers: [{ name: "X-Extra", credential: "api-token" }],
        },
      },
    ],
  };
}

function jsonResponse(body, init = {}) {
  return new Response(body == null ? null : JSON.stringify(body), {
    status: init.status ?? 200,
    headers: {
      "content-type": "application/json",
      ...(init.headers ?? {}),
    },
  });
}

function createAdapter() {
  const requests = [];
  const fetchImpl = async (input, init = {}) => {
    const url = new URL(String(input));
    const bodyText = typeof init.body === "string" ? init.body : undefined;
    const request = {
      method: init.method ?? "GET",
      url: String(input),
      pathname: url.pathname,
      headers: Object.fromEntries(new Headers(init.headers).entries()),
      body: bodyText ? JSON.parse(bodyText) : undefined,
    };
    requests.push(request);

    if (request.pathname === "/credential-vault") {
      if (request.method === "DELETE") {
        return new Response(null, { status: 204 });
      }
      return jsonResponse(stateResponse());
    }
    if (request.pathname === "/credential-vault/credentials") {
      return jsonResponse({
        revision: 9,
        credentials: [stateResponse().credentials[0]],
      });
    }
    if (request.pathname === "/credential-vault/credentials/api-token%2Fprimary") {
      return jsonResponse(stateResponse().credentials[0]);
    }
    if (request.pathname === "/credential-vault/bindings") {
      return jsonResponse({
        revision: 9,
        bindings: [stateResponse().bindings[0]],
      });
    }
    if (request.pathname === "/credential-vault/bindings/api%20binding") {
      return jsonResponse(stateResponse().bindings[0]);
    }
    return jsonResponse({ code: "NOT_FOUND", message: "not found" }, { status: 404 });
  };

  const policyClient = {
    GET() {
      throw new Error("policy GET was not expected");
    },
    PATCH() {
      throw new Error("policy PATCH was not expected");
    },
    DELETE() {
      throw new Error("policy DELETE was not expected");
    },
  };

  return {
    adapter: new EgressAdapter(policyClient, {
      baseUrl: "https://egress.example",
      fetch: fetchImpl,
      headers: {
        "OPEN-SANDBOX-API-KEY": "sdk-key",
        "x-endpoint-token": "route-token",
      },
    }),
    requests,
  };
}

test("EgressAdapter sends Credential Vault JSON payloads with endpoint headers", async () => {
  const { adapter, requests } = createAdapter();

  const created = await adapter.create({
    credentials: [credential],
    bindings: [binding],
  });

  await adapter.patch({
    expectedRevision: 9,
    credentials: {
      add: [credential],
      replace: [credential],
      delete: ["old-token"],
    },
    bindings: {
      add: [binding],
      replace: [binding],
      delete: ["old-binding"],
    },
  });
  const current = await adapter.get();
  const credentials = await adapter.listCredentials();
  const oneCredential = await adapter.getCredential("api-token/primary");
  const bindings = await adapter.listBindings();
  const oneBinding = await adapter.getBinding("api binding");
  await adapter.delete();

  assert.deepEqual(created, {
    revision: 9,
    credentials: [sanitizedCredential],
    bindings: [sanitizedBinding],
  });
  assert.deepEqual(current, created);
  assert.deepEqual(credentials, [sanitizedCredential]);
  assert.deepEqual(oneCredential, sanitizedCredential);
  assert.deepEqual(bindings, [sanitizedBinding]);
  assert.deepEqual(oneBinding, sanitizedBinding);

  assert.deepEqual(
    requests.map((request) => [request.method, request.pathname]),
    [
      ["POST", "/credential-vault"],
      ["PATCH", "/credential-vault"],
      ["GET", "/credential-vault"],
      ["GET", "/credential-vault/credentials"],
      ["GET", "/credential-vault/credentials/api-token%2Fprimary"],
      ["GET", "/credential-vault/bindings"],
      ["GET", "/credential-vault/bindings/api%20binding"],
      ["DELETE", "/credential-vault"],
    ],
  );
  assert.equal(requests[0].headers["open-sandbox-api-key"], "sdk-key");
  assert.equal(requests[0].headers["x-endpoint-token"], "route-token");
  assert.equal(requests[0].headers["content-type"], "application/json");
  assert.equal(requests[2].headers["content-type"], undefined);
  assert.deepEqual(requests[0].body, {
    credentials: [credential],
    bindings: [binding],
  });
  assert.deepEqual(requests[1].body, {
    expectedRevision: 9,
    credentials: {
      add: [credential],
      replace: [credential],
      delete: ["old-token"],
    },
    bindings: {
      add: [binding],
      replace: [binding],
      delete: ["old-binding"],
    },
  });
});

test("Credential Vault state omits plaintext secret fields", async () => {
  const { adapter } = createAdapter();
  const state = await adapter.get();

  assert.equal(Object.hasOwn(state.credentials[0], "source"), false);
  assert.equal(Object.hasOwn(state.bindings[0].auth, "credential"), false);
  assert.equal(Object.hasOwn(state.bindings[0].auth, "headers"), false);

  const dts = await readDistDeclarations();
  const inlineCredentialSource = declarationBlock(dts, "InlineCredentialSource");
  const credentialMetadata = declarationBlock(dts, "CredentialMetadata");
  const authMetadata = declarationBlock(dts, "CredentialAuthMetadata");
  const bindingMetadata = declarationBlock(dts, "CredentialBindingMetadata");

  assert.match(inlineCredentialSource, /\btype\?: "inline"/);
  assert.doesNotMatch(credentialMetadata, /extends Record/);
  assert.doesNotMatch(credentialMetadata, /\bsource\s*[?:]/);
  assert.doesNotMatch(authMetadata, /extends Record/);
  assert.doesNotMatch(authMetadata, /\bcredential\s*[?:]/);
  assert.doesNotMatch(authMetadata, /\bheaders\s*[?:]/);
  assert.doesNotMatch(bindingMetadata, /extends Record/);
  assert.match(bindingMetadata, /\bauth\??: CredentialAuthMetadata/);
});

test("Egress declarations keep Credential Vault optional for custom adapters", async () => {
  const dts = await readDistDeclarations();
  const egress = declarationBlock(dts, "Egress");
  const egressStack = declarationBlock(dts, "EgressStack");

  assert.doesNotMatch(egress, /extends CredentialVault/);
  assert.doesNotMatch(egress, /\bcreate\(/);
  assert.match(egressStack, /\begress: Egress/);
  assert.match(egressStack, /\bcredentialVault\??: CredentialVault/);
});

async function readDistDeclarations() {
  const distDir = new URL("../dist/", import.meta.url);
  const entries = await readdir(distDir);
  const declarations = entries.filter((entry) => entry.endsWith(".d.ts"));
  const contents = await Promise.all(
    declarations.map((entry) => readFile(new URL(entry, distDir), "utf8")),
  );
  return contents.join("\n");
}

function declarationBlock(dts, name) {
  const start = dts.indexOf(`interface ${name}`);
  assert.notEqual(start, -1, `missing ${name} declaration`);
  const end = dts.indexOf("\n}", start);
  assert.notEqual(end, -1, `missing ${name} declaration end`);
  return dts.slice(start, end + 2);
}
