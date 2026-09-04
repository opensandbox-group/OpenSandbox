import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

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

test("public type declarations export credential substitution models", async () => {
  const declarations = await readFile(new URL("../dist/index.d.ts", import.meta.url), "utf8");

  assert.match(declarations, /\bCredentialSubstitution\b/);
  assert.match(declarations, /\bCredentialSubstitutionSurface\b/);
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
  assert.equal(sdk.PoolHealthState.HEALTHY, "HEALTHY");
  assert.equal(sdk.PooledSandboxCreateReason.WARMUP, "WARMUP");
  const exportedNames = exportedDeclarationNames(declarations);
  assert.equal(exportedNames.has("SandboxPoolOptions"), true);
  assert.equal(exportedNames.has("PoolStateStore"), true);
  assert.equal(exportedNames.has("PoolSnapshot"), true);
  assert.equal(exportedNames.has("PoolSandboxPreparer"), true);
});
