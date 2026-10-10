// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile, realpath, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const packageDir = fileURLToPath(new URL('../', import.meta.url));
const scratch = await mkdtemp(join(tmpdir(), 'opensandbox-dsh-pack-'));
const manifest = JSON.parse(await readFile(join(packageDir, 'package.json'), 'utf8'));
const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const run = (command, args, cwd) => execFileSync(command, args, {
  cwd, encoding: 'utf8', timeout: 180_000, maxBuffer: 8 * 1024 * 1024,
  env: {...process.env, npm_config_cache: join(tmpdir(), 'opensandbox-dsh-pack-npm-cache')},
});
try {
  const packed = JSON.parse(run(npm, ['pack', '--json', '--ignore-scripts', '--pack-destination', scratch], packageDir))[0];
  const paths = packed.files.map(file => file.path).sort();
  // Deliberately closed module set: stale compiled fixture/credential modules must fail.
  const modules = ['binding', 'errors', 'filesystem', 'headless', 'helper-client', 'index', 'output', 'sdk-transport', 'shell', 'types'];
  const expected = ['package.json', 'README.md', 'LICENSE', 'remote/fs_helper.py',
    ...modules.flatMap(name => [`dist/${name}.js`, `dist/${name}.d.ts`])].sort();
  const unexpected = paths.filter(path => !expected.includes(path));
  assert.equal(unexpected.length, 0, `Unexpected tarball files: ${unexpected.join(', ')}`);
  assert.deepEqual(paths, expected, 'Tarball must contain exactly the approved artifacts');
  assert(paths.includes('remote/fs_helper.py'), 'Python helper is missing');
  for (const entry of Object.values(manifest.exports)) {
    assert(paths.includes(entry.types.slice(2)), `Missing declarations: ${entry.types}`);
    assert(paths.includes(entry.import.slice(2)), `Missing ESM export: ${entry.import}`);
  }
  console.log(`tarball allowlist: PASS (${paths.length} exact approved files; no unexpected compiled artifacts)`);
  await writeFile(join(scratch, 'package.json'), JSON.stringify({
    name: 'isolated-dsh-tarball-consumer', private: true, type: 'module',
    dependencies: { [manifest.name]: `file:${join(scratch, packed.filename)}`, typescript: '5.9.3', '@types/node': '24.10.0' },
  }, null, 2));
  run(npm, ['install', '--ignore-scripts', '--strict-peer-deps', '--no-audit', '--no-fund',
    '--registry=https://registry.npmjs.org', `--cache=${join(tmpdir(), 'opensandbox-dsh-pack-npm-cache')}`], scratch);
  const installed = join(scratch, 'node_modules', '@opensandbox', 'deepseek-harness');
  assert.equal(await realpath(installed), installed, 'Consumer must install tarball, not a source symlink');
  const sourceHelper = await readFile(join(packageDir, 'remote', 'fs_helper.py'));
  const packedHelper = await readFile(join(installed, 'remote', 'fs_helper.py'));
  assert.equal(createHash('sha256').update(packedHelper).digest('hex'), createHash('sha256').update(sourceHelper).digest('hex'));
  for (const [name, version] of Object.entries(manifest.dependencies)) {
    const actual = JSON.parse(await readFile(join(scratch, 'node_modules', name, 'package.json'), 'utf8'));
    assert.equal(actual.version, version, `Wrong pinned dependency: ${name}`);
  }
  await writeFile(join(scratch, 'consumer.mjs'), `
import assert from 'node:assert/strict';
import * as api from '@opensandbox/deepseek-harness';
import { openBinding } from '@opensandbox/deepseek-harness/binding';
import { SdkTransport } from '@opensandbox/deepseek-harness/sdk-transport';
import { RemoteShell } from '@opensandbox/deepseek-harness/shell';
import { RemoteFileSystem } from '@opensandbox/deepseek-harness/filesystem';
import { createHeadlessSession } from '@opensandbox/deepseek-harness/headless';
for (const [name, value] of Object.entries({ openBinding, SdkTransport, RemoteShell, RemoteFileSystem, createHeadlessSession })) {
  assert.equal(typeof value, 'function'); assert.equal(api[name], value);
}
await assert.rejects(import('@opensandbox/deepseek-harness/src/index.ts'), {code: 'ERR_PACKAGE_PATH_NOT_EXPORTED'});
`);
  run(process.execPath, ['consumer.mjs'], scratch);
  await writeFile(join(scratch, 'consumer.ts'), `
import { createHeadlessSession, openBinding, SdkTransport, RemoteShell, RemoteFileSystem } from '@opensandbox/deepseek-harness';
import type { BoundSandbox, HeadlessSessionOptions, OpenOptions, ByteReadOptions, BindingDescriptor } from '@opensandbox/deepseek-harness';
import { createHeadlessSession as subpathHeadless } from '@opensandbox/deepseek-harness/headless';
import { LlmAdapter } from '@deepseek-ai/dsh-llm';
import type { GenerateOptions, StreamChunk } from '@deepseek-ai/dsh-llm';
class Adapter extends LlmAdapter { async *stream(_options: GenerateOptions): AsyncIterable<StreamChunk> { yield { type: 'finish', reason: {kind: 'stop'} }; } }
declare const binding: BoundSandbox;
const options: HeadlessSessionOptions = {binding, adapter: new Adapter(), provider: 'explicit', model: 'explicit'};
const headless: typeof createHeadlessSession = subpathHeadless;
const descriptor: BindingDescriptor = binding.descriptor;
const read: ByteReadOptions = {};
const opener: (options: OpenOptions) => Promise<BoundSandbox> = openBinding;
void [headless, options, descriptor, read, opener, SdkTransport, RemoteShell, RemoteFileSystem];
`);
  await writeFile(join(scratch, 'tsconfig.json'), JSON.stringify({compilerOptions: {
    target: 'ES2022', module: 'NodeNext', moduleResolution: 'NodeNext', strict: true,
    exactOptionalPropertyTypes: true, skipLibCheck: true, noEmit: true, types: ['node'],
  }, include: ['consumer.ts']}));
  run(process.execPath, [join(dirname(installed), '..', 'typescript', 'bin', 'tsc'), '-p', 'tsconfig.json'], scratch);
  console.log('isolated tarball import, helper asset and TypeScript consumer: PASS');
} finally {
  await rm(scratch, {recursive: true, force: true});
}
