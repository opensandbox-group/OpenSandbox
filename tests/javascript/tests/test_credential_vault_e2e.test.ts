// Copyright 2026 Alibaba Group Holding Ltd.
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

import { expect, test } from "vitest";

import { Sandbox, type Credential, type CredentialAuth, type CredentialBinding } from "@alibaba-group/opensandbox";

import { createConnectionConfig, getSandboxImage } from "./base_e2e.ts";

const DEFAULT_TARGET_HOST = "credential-vault-e2e.opensandbox.test";

const SECRET_VALUES: Record<string, string> = {
  "bearer-token": "vault-bearer-token",
  "basic-token": "dXNlcjpwYXNz",
  "api-key-token": "vault-api-key-token",
  "client-id": "vault-client-id",
  "client-secret": "vault-client-secret",
  "runtime-token": "vault-runtime-token",
  "runtime-token-replaced": "vault-runtime-token-replaced",
};

const credentialVaultTest = process.env.OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP ? test : test.skip;

credentialVaultTest("credential vault injects all auth types", async () => {
  const targetIp = credentialVaultTargetIp();
  const sandbox = await createCredentialVaultSandbox(targetIp);

  try {
    const state = await sandbox.credentialVault.create({
      credentials: credentialVaultCredentials(
        "bearer-token",
        "basic-token",
        "api-key-token",
        "client-id",
        "client-secret",
        "runtime-token",
        "runtime-token-replaced",
      ),
      bindings: [
        credentialVaultBinding("bearer", "/bearer", { type: "bearer", credential: "bearer-token" }),
        credentialVaultBinding("basic", "/basic", { type: "basic", credential: "basic-token" }),
        credentialVaultBinding("api-key", "/api-key", {
          type: "apiKey",
          name: "X-Api-Key",
          credential: "api-key-token",
        }),
        credentialVaultBinding("custom-headers", "/custom-headers", {
          type: "customHeaders",
          headers: [
            { name: "X-Client-Id", credential: "client-id" },
            { name: "X-Client-Secret", credential: "client-secret" },
          ],
        }),
      ],
    });

    const statePayload = JSON.stringify(state);
    for (const secret of Object.values(SECRET_VALUES)) {
      expect(statePayload).not.toContain(secret);
    }
    expect(new Set(state.bindings.map((binding) => binding.auth?.type))).toEqual(
      new Set(["bearer", "basic", "apiKey", "customHeaders"]),
    );

    for (const path of ["/bearer", "/basic", "/api-key", "/custom-headers"]) {
      const response = await curlJson(sandbox, targetIp, path);
      expect(response.ok).toBe(true);
      expect(response.case).toBe(path.slice(1));
      expect(response.missingOrInvalid).toEqual([]);
    }
  } finally {
    await killSandbox(sandbox);
  }
}, 5 * 60_000);

credentialVaultTest("credential vault runtime mutation adds replaces and deletes binding", async () => {
  const targetIp = credentialVaultTargetIp();
  const sandbox = await createCredentialVaultSandbox(targetIp);

  try {
    let state = await sandbox.credentialVault.create({ credentials: [], bindings: [] });
    expect(state.revision).toBe(1);
    expect(state.credentials).toEqual([]);
    expect(state.bindings).toEqual([]);

    state = await sandbox.credentialVault.patch({
      expectedRevision: state.revision,
      credentials: {
        add: [credentialVaultCredential("runtime-token", "runtime-token")],
      },
      bindings: {
        add: [
          credentialVaultBinding("runtime-added", "/runtime-added", {
            type: "apiKey",
            name: "X-Runtime-Token",
            credential: "runtime-token",
          }),
        ],
      },
    });
    expect(state.revision).toBe(2);
    expect(state.credentials.map((credential) => credential.name)).toEqual(["runtime-token"]);
    expect(state.bindings.map((binding) => binding.name)).toEqual(["runtime-added"]);
    expect(JSON.stringify(state)).not.toContain(SECRET_VALUES["runtime-token"]);

    let response = await curlJson(sandbox, targetIp, "/runtime-added");
    expect(response.ok).toBe(true);
    expect(response.case).toBe("runtime-added");
    expect(response.missingOrInvalid).toEqual([]);

    state = await sandbox.credentialVault.patch({
      expectedRevision: state.revision,
      bindings: { delete: ["runtime-added"] },
    });
    expect(state.revision).toBe(3);
    expect(state.bindings).toEqual([]);

    state = await sandbox.credentialVault.patch({
      expectedRevision: state.revision,
      credentials: {
        replace: [credentialVaultCredential("runtime-token", "runtime-token-replaced")],
      },
      bindings: {
        add: [
          credentialVaultBinding("runtime-replaced", "/runtime-replaced", {
            type: "apiKey",
            name: "X-Runtime-Token",
            credential: "runtime-token",
          }),
        ],
      },
    });
    expect(state.revision).toBe(4);
    expect(state.credentials.map((credential) => credential.name)).toEqual(["runtime-token"]);
    expect(state.bindings.map((binding) => binding.name)).toEqual(["runtime-replaced"]);

    const statePayload = JSON.stringify(state);
    expect(statePayload).not.toContain(SECRET_VALUES["runtime-token"]);
    expect(statePayload).not.toContain(SECRET_VALUES["runtime-token-replaced"]);

    response = await curlJson(sandbox, targetIp, "/runtime-replaced");
    expect(response.ok).toBe(true);
    expect(response.case).toBe("runtime-replaced");
    expect(response.missingOrInvalid).toEqual([]);

    response = await curlJson(sandbox, targetIp, "/runtime-added", false);
    expect(response.ok).toBe(false);
    expect(response.case).toBe("runtime-added");
    expect(response.missingOrInvalid).toEqual(["x-runtime-token"]);

    state = await sandbox.credentialVault.patch({
      expectedRevision: state.revision,
      bindings: { delete: ["runtime-replaced"] },
    });
    expect(state.revision).toBe(5);
    expect(state.bindings).toEqual([]);

    state = await sandbox.credentialVault.patch({
      expectedRevision: state.revision,
      credentials: { delete: ["runtime-token"] },
    });
    expect(state.revision).toBe(6);
    expect(state.credentials).toEqual([]);
  } finally {
    await killSandbox(sandbox);
  }
}, 5 * 60_000);

function credentialVaultTargetHost(): string {
  return process.env.OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST ?? DEFAULT_TARGET_HOST;
}

function credentialVaultTargetIp(): string {
  const targetIp = process.env.OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP;
  if (!targetIp) {
    throw new Error("set OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP to run Credential Vault E2E");
  }
  return targetIp;
}

async function createCredentialVaultSandbox(targetIp: string): Promise<Sandbox> {
  const image = process.env.OPENSANDBOX_CREDENTIAL_VAULT_E2E_SANDBOX_IMAGE ?? getSandboxImage();
  return Sandbox.create({
    connectionConfig: createConnectionConfig(),
    image,
    resource: {
      cpu: process.env.OPENSANDBOX_E2E_SANDBOX_CPU ?? "1",
      memory: process.env.OPENSANDBOX_E2E_SANDBOX_MEMORY ?? "2Gi",
    },
    readyTimeoutSeconds: 90,
    timeoutSeconds: 5 * 60,
    networkPolicy: {
      defaultAction: "deny",
      egress: [
        { action: "allow", target: credentialVaultTargetHost() },
        { action: "allow", target: targetIp },
      ],
    },
    credentialProxy: { enabled: true },
    metadata: {
      [process.env.OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY ?? "opensandbox.e2e"]:
        process.env.OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE ?? "credential-vault",
    },
  });
}

function credentialVaultCredentials(...names: string[]): Credential[] {
  return names.map((name) => credentialVaultCredential(name, name));
}

function credentialVaultCredential(name: string, valueName: string): Credential {
  return {
    name,
    source: {
      type: "inline",
      value: SECRET_VALUES[valueName],
    },
  };
}

function credentialVaultBinding(name: string, path: string, auth: CredentialAuth): CredentialBinding {
  return {
    name,
    match: {
      schemes: ["http"],
      ports: [80],
      hosts: [credentialVaultTargetHost()],
      methods: ["GET"],
      paths: [path],
    },
    auth,
  };
}

async function curlJson(
  sandbox: Sandbox,
  targetIp: string,
  path: string,
  failOnHttpError = true,
): Promise<Record<string, unknown>> {
  const failFlag = failOnHttpError ? "--fail " : "";
  const command =
    `curl ${failFlag}--silent --show-error --connect-timeout 5 --max-time 20 ` +
    `--resolve ${credentialVaultTargetHost()}:80:${targetIp} ` +
    `http://${credentialVaultTargetHost()}${path}`;
  for (const secret of Object.values(SECRET_VALUES)) {
    expect(command).not.toContain(secret);
  }

  const result = await sandbox.commands.run(command);
  expect(result.error).toBeUndefined();
  expect(result.exitCode).toBe(0);
  const stdout = result.logs.stdout.map((part) => part.text).join("");
  expect(stdout).not.toBe("");
  return JSON.parse(stdout) as Record<string, unknown>;
}

async function killSandbox(sandbox: Sandbox): Promise<void> {
  try {
    await sandbox.kill();
  } finally {
    await sandbox.close();
  }
}
