#!/usr/bin/env node

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

import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const LICENSE_OWNER = "Alibaba Group Holding Ltd.";
const LICENSE_MARKER_REGEX = new RegExp(`Copyright [0-9]{4} ${LICENSE_OWNER}`);

function buildLicenseText() {
  const year = new Date().getFullYear();
  return `Copyright ${year} ${LICENSE_OWNER}.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.`;
}

function asLineCommentHeader(text) {
  return text
    .split("\n")
    .map((line) => `// ${line}`)
    .join("\n");
}

function ensureLicenseHeader(filePath) {
  const body = readFileSync(filePath, "utf8");
  const head = body.split("\n").slice(0, 40).join("\n");
  if (LICENSE_MARKER_REGEX.test(head)) {
    return;
  }
  const header = asLineCommentHeader(buildLicenseText());
  writeFileSync(filePath, `${header}\n\n${body}`, "utf8");
}

function fail(message) {
  console.error(`❌ ${message}`);
  process.exit(1);
}

function run(cmd, args, cwd) {
  const pretty = [cmd, ...args].join(" ");
  console.log(`\n▶ ${pretty}`);
  const res = spawnSync(cmd, args, { cwd, stdio: "inherit" });
  if (res.status !== 0) {
    fail(`Command failed (exit=${res.status}): ${pretty}`);
  }
}

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

// scripts/ -> package root
const packageRoot = path.resolve(__dirname, "..");
// scripts/ -> repo root (OpenSandbox/)
const repoRoot = path.resolve(__dirname, "../../../../");

const specs = {
  execd: path.join(repoRoot, "specs", "execd-api.yaml"),
  egress: path.join(repoRoot, "specs", "egress-api.yaml"),
  lifecycle: path.join(repoRoot, "specs", "sandbox-lifecycle.yml"),
};

for (const [name, p] of Object.entries(specs)) {
  if (!existsSync(p)) {
    fail(`OpenAPI spec not found for '${name}': ${p}`);
  }
}

const outDir = path.join(packageRoot, "src", "api");
mkdirSync(outDir, { recursive: true });

const outFiles = {
  execd: path.join(outDir, "execd.ts"),
  egress: path.join(outDir, "egress.ts"),
  lifecycle: path.join(outDir, "lifecycle.ts"),
};

console.log("🚀 OpenSandbox TypeScript SDK API Generator");
console.log(`- repoRoot: ${repoRoot}`);
console.log(`- outDir:   ${outDir}`);

// Use pnpm as requested by the project rules.
run("pnpm", ["exec", "openapi-typescript", specs.execd, "-o", outFiles.execd], packageRoot);
run("pnpm", ["exec", "openapi-typescript", specs.egress, "-o", outFiles.egress], packageRoot);
run(
  "pnpm",
  ["exec", "openapi-typescript", specs.lifecycle, "-o", outFiles.lifecycle],
  packageRoot,
);

// The generator may overwrite outputs; re-apply unified license headers after generation.
ensureLicenseHeader(outFiles.execd);
ensureLicenseHeader(outFiles.egress);
ensureLicenseHeader(outFiles.lifecycle);

// Clarify that the generated session API in execd.ts is not the recommended entry point.
const EXECD_SESSION_NOTE = `/**
 * NOTE: The session-related path types and operations in this file (e.g. /session, runInSession)
 * are generated from the execd OpenAPI spec. They are not the recommended runtime entry point.
 * Use \`sandbox.commands.createSession()\`, \`sandbox.commands.runInSession()\`, and
 * \`sandbox.commands.deleteSession()\` instead.
 */`;

function ensureExecdSessionNote(filePath) {
  const body = readFileSync(filePath, "utf8");
  if (body.includes("not the recommended runtime entry point")) {
    return;
  }
  // Insert after the first "Do not make direct changes" block (after the first empty line that follows it).
  const marker = "Do not make direct changes to the file.";
  const idx = body.indexOf(marker);
  if (idx === -1) return;
  const afterBlock = body.indexOf("\n\n", idx + marker.length);
  const insertAt = afterBlock === -1 ? idx + marker.length : afterBlock + 2;
  const newBody = body.slice(0, insertAt) + "\n" + EXECD_SESSION_NOTE + "\n" + body.slice(insertAt);
  writeFileSync(filePath, newBody, "utf8");
}

ensureExecdSessionNote(outFiles.execd);

console.log("\n✅ API type generation completed:");
console.log(`- ${path.relative(packageRoot, outFiles.execd)}`);
console.log(`- ${path.relative(packageRoot, outFiles.egress)}`);
console.log(`- ${path.relative(packageRoot, outFiles.lifecycle)}`);

