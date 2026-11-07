// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from 'node:child_process';
import { cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { root, stage } from './stage.mjs';

await stage();
const check = process.argv.includes('--check');
if (!check) await rm(new URL('../dist/', import.meta.url), {recursive: true, force: true});
execFileSync(process.execPath, [createRequire(import.meta.url).resolve('typescript/bin/tsc'),
  '-p', check ? 'tsconfig.json' : 'tsconfig.build.json', ...(check ? ['--noEmit'] : [])], {cwd: root, stdio: 'inherit'});
if (!check) {
  // DSH's browser loader consumes its ModuleLoader factory format, not Node ESM.
  // Keep this deliberately dependency-free client separate from all host modules.
  const client = ts.transpileModule(await readFile(new URL('../src/client.ts', import.meta.url), 'utf8'), {
    compilerOptions: {target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS},
  }).outputText;
  if (/\brequire\s*\(/.test(client)) throw new Error('Native browser client must not import a runtime dependency.');
  await writeFile(new URL('../dist/client.js', import.meta.url),
    '// Copyright 2026 The OpenSandbox Authors\n// SPDX-License-Identifier: Apache-2.0\n' +
    'window.__ModuleLoader__.load({id: "@opensandbox/deepseek-harness-native", factory: () => {\n' +
    'const exports = {};\n' + client + '\nreturn exports;\n}});\n');
  await mkdir(new URL('../dist/remote/', import.meta.url), {recursive: true});
  await cp(new URL('../../remote/fs_helper.py', import.meta.url), new URL('../dist/remote/fs_helper.py', import.meta.url));
}
