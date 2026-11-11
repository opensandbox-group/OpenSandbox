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

import { afterAll, beforeAll, expect, test } from "vitest";
import { randomUUID } from "node:crypto";
import { Sandbox, SandboxManager, SandboxApiException, type DiagnosticContent } from "@alibaba-group/opensandbox";
import { createConnectionConfig, getSandboxImage } from "./base_e2e.ts";

const marker = "opensandbox-diagnostics-e2e";
let manager: SandboxManager;
let sandbox: Sandbox | undefined;

function assertContent(result: DiagnosticContent, kind: string, scope: string): void {
  expect(result.sandboxId).toBe(sandbox!.id);
  expect(result.kind).toBe(kind);
  expect(result.scope).toBe(scope);
  expect(result.delivery).toBe("inline");
  expect(result.contentType).toMatch(/^text\/plain/);
  expect(typeof result.content).toBe("string");
  expect(typeof result.truncated).toBe("boolean");
}

beforeAll(async () => {
  manager = SandboxManager.create({ connectionConfig: createConnectionConfig() });
  sandbox = await Sandbox.create({
    connectionConfig: createConnectionConfig(),
    image: getSandboxImage(), timeoutSeconds: 300, readyTimeoutSeconds: 90,
    entrypoint: ["sh", "-c", `echo ${marker}; sleep 300`],
    env: { EXECD_API_GRACE_SHUTDOWN: "1s" },
  });
}, 180_000);

afterAll(async () => {
  try {
    if (sandbox) {
      try { await sandbox.kill(); }
      finally { await sandbox.close(); }
    }
  } finally { if (manager) await manager.close(); }
}, 60_000);

test("context identifies the same backend through manager and sandbox", async () => {
  const context = await manager.getDiagnosticContext(sandbox!.id);
  const current = await sandbox!.getDiagnosticContext();
  expect(context.sandboxId).toBe(sandbox!.id);
  expect(current.sandboxId).toBe(context.sandboxId);
  expect(current.backendType).toBe(context.backendType);
  expect(current.backendIdentifiers).toEqual(context.backendIdentifiers);
  expect(context.state).toBeTruthy();
  if (context.backendType === "docker") {
    expect(context.backendIdentifiers.containerId).toBeTruthy();
    expect(context.backendIdentifiers.containerName).toBeTruthy();
  } else {
    expect(context.backendType).toBe("kubernetes");
    expect(context.backendIdentifiers.namespace).toBeTruthy();
    expect(context.backendIdentifiers.workloadName).toBeTruthy();
  }
  console.info("diagnostic context", context);
});

test.each(["container", "all"])("logs (%s) contain entrypoint output", async (scope) => {
  for (const getLogs of [
    () => manager.getDiagnosticLogs(sandbox!.id, scope),
    () => sandbox!.getDiagnosticLogs(scope),
  ]) {
    const deadline = Date.now() + 30_000;
    for (;;) {
      const result = await getLogs();
      assertContent(result, "logs", scope);
      if (result.content!.includes(marker)) break;
      expect(Date.now(), `entrypoint log missing: ${result.content}`).toBeLessThan(deadline);
      await new Promise(resolve => setTimeout(resolve, 500));
    }
  }
}, 90_000);

test.each(["runtime", "all"])("events (%s) return diagnostic descriptors", async (scope) => {
  assertContent(await manager.getDiagnosticEvents(sandbox!.id, scope), "events", scope);
  assertContent(await sandbox!.getDiagnosticEvents(scope), "events", scope);
});

test.each(["logs", "events"])("unsupported %s scope preserves API error", async (kind) => {
  const call = kind === "logs"
    ? manager.getDiagnosticLogs(sandbox!.id, "unsupported-e2e-scope")
    : manager.getDiagnosticEvents(sandbox!.id, "unsupported-e2e-scope");
  await expect(call).rejects.toMatchObject({
    statusCode: 400, error: { code: "DIAGNOSTICS_SCOPE_UNSUPPORTED" },
  });
});

test.each(["context", "logs", "events"])("missing sandbox returns 404 for %s", async (kind) => {
  const id = randomUUID();
  const call = kind === "context" ? manager.getDiagnosticContext(id)
    : kind === "logs" ? manager.getDiagnosticLogs(id, "container")
    : manager.getDiagnosticEvents(id, "runtime");
  await expect(call).rejects.toBeInstanceOf(SandboxApiException);
  await expect(call).rejects.toMatchObject({ statusCode: 404 });
});
