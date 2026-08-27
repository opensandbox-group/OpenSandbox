import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("public type declarations export credential substitution models", async () => {
  const declarations = await readFile(new URL("../dist/index.d.ts", import.meta.url), "utf8");

  assert.match(declarations, /\bCredentialSubstitution\b/);
  assert.match(declarations, /\bCredentialSubstitutionSurface\b/);
});

test("public type declarations export lifecycle models", async () => {
  const declarations = await readFile(new URL("../dist/index.d.ts", import.meta.url), "utf8");
  const exportedNames = new Set(
    [...declarations.matchAll(/export\s*(?:type\s+)?\{([^}]*)\}/g)].flatMap(([, body]) =>
      body
        .split(",")
        .map((item) =>
          item
            .trim()
            .replace(/^type\s+/, "")
            .split(/\s+as\s+/)
            .at(-1),
        )
        .filter(Boolean),
    ),
  );
  for (const [, name] of declarations.matchAll(
    /export\s+(?:declare\s+)?(?:type|interface|class|const|function|enum)\s+([\w$]+)/g,
  )) {
    exportedNames.add(name);
  }

  assert.ok(exportedNames.size > 0, "failed to parse any export clause from dist/index.d.ts");
  assert.equal(exportedNames.has("LifecycleHook"), true);
  assert.equal(exportedNames.has("PeriodicLifecycleHook"), true);
  assert.equal(exportedNames.has("SandboxLifecycle"), true);
});
