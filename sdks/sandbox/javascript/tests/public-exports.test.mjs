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
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";
import ts from "typescript";

function exportedDeclarationNames(declarations) {
  const names = new Set(
    [...declarations.matchAll(/export\s*(?:type\s+)?\{([^}]*)\}/g)].flatMap(([, body]) =>
      body
        .split(",")
        .map((item) => item.trim().replace(/^type\s+/, "").split(/\s+as\s+/).at(-1))
        .filter(Boolean),
    ),
  );
  for (const [, name] of declarations.matchAll(
    /export\s+(?:declare\s+)?(?:type|interface|class|const|function|enum)\s+([\w$]+)/g,
  )) {
    names.add(name);
  }
  return names;
}

test("public type declarations export stable models", async () => {
  const declarations = await readFile(new URL("../dist/index.d.ts", import.meta.url), "utf8");

  assert.match(declarations, /\bCredentialSubstitution\b/);
  assert.match(declarations, /\bCredentialSubstitutionSurface\b/);
});

test("legacy filesystem adapters remain assignable to the public filesystem interface", () => {
  const filename = fileURLToPath(new URL("./filesystem-compatibility.mts", import.meta.url));
  const source = `
    import type { ByteRange, ReadBytesResponse, ReadBytesStream, Sandbox, SandboxFiles } from "../dist/index.js";
    type LegacyFiles = Omit<SandboxFiles, "readBytesDetailed" | "readBytesStreamDetailed">;
    function configure(files: LegacyFiles): Sandbox["files"] { return files; }
    async function download(files: SandboxFiles) {
      if (files.readBytesDetailed && files.readBytesStreamDetailed) {
        await files.readBytesDetailed("/data.bin");
        const response: ReadBytesResponse<ReadBytesStream> = await files.readBytesStreamDetailed("/data.bin");
        const range: ByteRange | undefined = response.contentRange;
        await response.body.close();
      }
    }
  `;
  const options = {
    strict: true,
    noEmit: true,
    skipLibCheck: true,
    target: ts.ScriptTarget.ES2022,
    module: ts.ModuleKind.NodeNext,
    moduleResolution: ts.ModuleResolutionKind.NodeNext,
  };
  const host = ts.createCompilerHost(options);
  const getSourceFile = host.getSourceFile;
  host.getSourceFile = (path, ...args) => path === filename
    ? ts.createSourceFile(path, source, options.target, true)
    : getSourceFile(path, ...args);
  const program = ts.createProgram([filename], options, host);
  const errors = ts.getPreEmitDiagnostics(program).map(
    (diagnostic) => ts.flattenDiagnosticMessageText(diagnostic.messageText, "\n"),
  );
  assert.deepEqual(errors, []);
});

test("public type declarations export lifecycle models", async () => {
  const declarations = await readFile(new URL("../dist/index.d.ts", import.meta.url), "utf8");
  const exportedNames = exportedDeclarationNames(declarations);

  assert.ok(exportedNames.size > 0, "failed to parse any export clause from dist/index.d.ts");
  assert.equal(exportedNames.has("LifecycleHook"), true);
  assert.equal(exportedNames.has("PeriodicLifecycleHook"), true);
  assert.equal(exportedNames.has("SandboxLifecycle"), true);
});

test("public package exports client-side pool APIs", async () => {
  const sdk = await import("../dist/index.js");
  const declarations = await readFile(new URL("../dist/index.d.ts", import.meta.url), "utf8");

  assert.equal(typeof sdk.SandboxPool, "function");
  assert.equal(typeof sdk.InMemoryPoolStateStore, "function");
  assert.equal(typeof sdk.PoolEmptyException, "function");
  assert.equal(typeof sdk.PoolAcquireFailedException, "function");
  assert.equal(typeof sdk.PoolNotRunningException, "function");
  assert.equal(typeof sdk.PoolStateStoreUnavailableException, "function");
  assert.equal(typeof sdk.AcquirePolicy, "object");
  assert.equal(sdk.AcquirePolicy.DIRECT_CREATE, "DIRECT_CREATE");
  assert.equal(sdk.PoolLifecycleState.RUNNING, "RUNNING");
  assert.equal(sdk.PoolState.HEALTHY, "HEALTHY");
  assert.equal(sdk.PoolDestroyState.ACTIVE, "ACTIVE");
  assert.equal(typeof sdk.SandboxPoolManager, "function");
  assert.equal(sdk.PooledSandboxCreateReason.WARMUP, "WARMUP");
  const exportedNames = exportedDeclarationNames(declarations);
  assert.equal(exportedNames.has("SandboxPoolOptions"), true);
  assert.equal(exportedNames.has("PoolStateStore"), true);
  assert.equal(exportedNames.has("PoolSnapshot"), true);
  assert.equal(exportedNames.has("PoolSandboxPreparer"), true);
});
