// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { runInNewContext } from 'node:vm';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { nativeModules, root, sharedModules } from './stage.mjs';

export async function pack(destination) {
  const result = execFileSync(process.platform === 'win32' ? 'npm.cmd' : 'npm',
    ['pack', '--json', '--ignore-scripts', '--pack-destination', destination],
    {cwd: root, encoding: 'utf8', timeout: 60_000, env: {...process.env, npm_config_cache: join(tmpdir(), 'opensandbox-native-npm-cache')}});
  const packed = JSON.parse(result)[0];
  const expected = [
    'LICENSE', 'README.md', 'package.json', 'cordis.patch.yml', 'examples/opensandbox-web.patch.yml',
    'dist/remote/fs_helper.py',
    ...nativeModules.flatMap(name => [`dist/${name}.js`, `dist/${name}.d.ts`]),
    ...sharedModules.flatMap(name => [`dist/core/${name}.js`, `dist/core/${name}.d.ts`]),
  ].sort();
  assert.deepEqual(packed.files.map(file => file.path).sort(), expected, 'Tarball must contain exactly the approved native artifacts');
  const manifest = JSON.parse(await readFile(join(root, 'package.json'), 'utf8'));
  assert.deepEqual(manifest.dependencies, {'@alibaba-group/opensandbox': '1.1.1'});
  assert.equal(manifest.dsh.bundle.patch, './cordis.patch.yml');
  assert.deepEqual(manifest.dsh.client, {platform: 'web', immediately: true});
  assert.equal(manifest.exports['./client'].default, './dist/client.js');
  // Exercise the actual browser artifact's loader contract, not a source alias.
  let clientModule;
  runInNewContext(await readFile(join(root, 'dist/client.js'), 'utf8'), {
    window: {__ModuleLoader__: {load(value) { clientModule = value; }}},
  });
  assert.equal(clientModule?.id, manifest.name);
  let upload;
  const client = clientModule.factory(() => { throw new Error('Unexpected browser dependency'); });
  client.apply({provide(name, value) { assert.equal(name, 'fileUpload'); upload = value; }});
  for (const method of ['upload', 'post']) {
    await assert.rejects(upload[method](new Proxy({}, {get() { throw new Error('Must not read input'); }})),
      /File uploads are disabled in the OpenSandbox profile\./);
  }
  assert.equal(manifest.peerDependencies['@deepseek-ai/dsh'], '0.2.1-alpha.1');
  assert.equal(manifest.overrides, undefined);
  assert.equal(manifest.pnpm?.overrides, undefined);
  for (const file of expected.filter(file => file.endsWith('.js'))) {
    const code = await readFile(join(root, file), 'utf8');
    assert(!code.includes('@opensandbox/deepseek-harness\''), 'Native package must not import embedded integration');
    assert(!code.includes('createHeadlessSession'), 'Native package must not contain an embedded Agent composition');
    for (const match of code.matchAll(/(?:from\s+|import\s*\()(['"])(@[^'"]+)\1/g)) {
      const name = match[2].split('/').slice(0, 2).join('/');
      assert(manifest.peerDependencies[name] || manifest.dependencies[name], `Undeclared runtime dependency ${name} in ${file}`);
    }
  }
  const sha = bytes => createHash('sha256').update(bytes).digest('hex');
  assert.equal(sha(await readFile(join(root, 'dist/remote/fs_helper.py'))), sha(await readFile(join(root, '../remote/fs_helper.py'))));
  for (const name of sharedModules) {
    assert.equal(sha(await readFile(join(root, `.native-build/src/core/${name}.ts`))), sha(await readFile(join(root, `../src/${name}.ts`))), `Shared source fork: ${name}`);
  }
  console.log(`native artifact allowlist, shared-source/helper parity, and peer-only manifest: PASS (${expected.length} files)`);
  return join(destination, packed.filename);
}

/** Compile actual packed declarations against explicitly supplied compatible host
 * dependencies. This is not a standalone npm install or the native runtime test. */
async function checkDeclarations(tarball, scratch) {
  const extracted = join(scratch, 'extracted');
  const consumer = join(scratch, 'consumer');
  await mkdir(extracted); await mkdir(consumer);
  execFileSync('tar', ['-xzf', tarball, '-C', extracted]);
  const manifest = JSON.parse(await readFile(join(root, 'package.json'), 'utf8'));
  const links = { ...manifest.dependencies, ...manifest.devDependencies };
  for (const name of Object.keys(links)) {
    const destination = join(consumer, 'node_modules', name);
    await mkdir(dirname(destination), {recursive: true});
    await symlink(join(root, 'node_modules', name), destination, 'dir');
  }
  const entry = join(consumer, 'node_modules', manifest.name);
  await mkdir(dirname(entry), {recursive: true});
  await symlink(join(extracted, 'package'), entry, 'dir');
  await writeFile(join(consumer, 'package.json'), '{"private":true,"type":"module"}\n');
  await writeFile(join(consumer, 'consumer.ts'), `
import native, { Config, NativeOwner, validateNativeConfig } from '@opensandbox/deepseek-harness-native';
import type { NativeConfig, NativeRoute, NativeStatus, NativeOwnerDependencies } from '@opensandbox/deepseek-harness-native';
import { Context } from '@deepseek-ai/cordis';
const config: NativeConfig = {domain: 'example.invalid', protocol: 'https', kind: 'create-template', templateId: 'explicit'};
const resolved = validateNativeConfig(config);
const dependencies: NativeOwnerDependencies = {};
const owner = new NativeOwner(new Context(), config, () => false, dependencies);
declare const route: NativeRoute;
const status: NativeStatus = route.status();
// @ts-expect-error The native artifact must not expose embedded/headless composition.
import { createHeadlessSession } from '@opensandbox/deepseek-harness-native';
void [native, Config, owner, resolved, status, createHeadlessSession];
`);
  await writeFile(join(consumer, 'tsconfig.json'), JSON.stringify({compilerOptions: {
    target: 'ES2022', module: 'NodeNext', moduleResolution: 'NodeNext', strict: true,
    exactOptionalPropertyTypes: true, preserveSymlinks: true, skipLibCheck: true, noEmit: true, types: ['node'],
  }, include: ['consumer.ts']}));
  execFileSync(process.execPath, [createRequire(import.meta.url).resolve('typescript/bin/tsc'), '-p', 'tsconfig.json'], {cwd: consumer, stdio: 'inherit'});
  console.log('packed declaration consumer with explicit compatible host dependencies: PASS');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const scratch = await mkdtemp(join(tmpdir(), 'opensandbox-native-pack-'));
  try { await checkDeclarations(await pack(scratch), scratch); } finally { await rm(scratch, {recursive: true, force: true}); }
}
