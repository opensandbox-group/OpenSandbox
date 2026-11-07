// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { cp, mkdir, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

export const root = fileURLToPath(new URL('../', import.meta.url));
export const sharedModules = Object.freeze([
  'binding', 'shell', 'filesystem', 'helper-client', 'sdk-transport', 'errors', 'output', 'types',
]);
export const nativeModules = Object.freeze(['config', 'owner', 'index', 'services', 'tools', 'client']);

/** This disposable tree is the only copy of shared provider sources. Never maintain a fork. */
export async function stage() {
  const output = new URL('../.native-build/', import.meta.url);
  await rm(output, {recursive: true, force: true});
  await mkdir(new URL('src/core/', output), {recursive: true});
  const inputs = (await readdir(new URL('../src/', import.meta.url))).sort();
  assert.deepEqual(inputs, nativeModules.map(name => `${name}.ts`).sort(), 'Unexpected or missing native source input');
  for (const name of nativeModules) {
    await cp(new URL(`../src/${name}.ts`, import.meta.url), new URL(`src/${name}.ts`, output));
  }
  for (const name of sharedModules) {
    const bytes = await readFile(new URL(`../../src/${name}.ts`, import.meta.url));
    await writeFile(new URL(`src/core/${name}.ts`, output), bytes);
  }
  await cp(new URL('../tests/', import.meta.url), new URL('tests/', output), {recursive: true});
  // Reuse offline SDK test fixtures, changing only their staged core-module path.
  await mkdir(new URL('tests/core-fixtures/', output), {recursive: true});
  for (const name of ['helper-sdk', 'sdk-facade', 'execd-lines']) {
    const source = await readFile(new URL(`../../tests/fixtures/${name}.ts`, import.meta.url), 'utf8');
    await writeFile(new URL(`tests/core-fixtures/${name}.ts`, output), source.replaceAll('../../src/', '../../src/core/'));
  }
  // The helper path is relative to core/helper-client.js in both staged tests and dist.
  await mkdir(new URL('src/remote/', output), {recursive: true});
  await cp(new URL('../../remote/fs_helper.py', import.meta.url), new URL('src/remote/fs_helper.py', output));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await stage();
