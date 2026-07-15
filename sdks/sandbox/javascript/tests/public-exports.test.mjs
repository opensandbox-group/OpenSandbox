import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

test("public type declarations export credential substitution models", async () => {
  const declarations = await readFile(new URL("../dist/index.d.ts", import.meta.url), "utf8");

  assert.match(declarations, /\bCredentialSubstitution\b/);
  assert.match(declarations, /\bCredentialSubstitutionSurface\b/);
});
