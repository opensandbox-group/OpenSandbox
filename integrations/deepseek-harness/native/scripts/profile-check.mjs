// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cp, mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { parse } from 'yaml';
import ts from 'typescript';
import { pack } from './pack-check.mjs';
import { root } from './stage.mjs';

const require = createRequire(import.meta.url);
const cli = require.resolve('@deepseek-ai/dsh/lib/bin.js');
const scratch = await mkdtemp(join(tmpdir(), 'opensandbox-native-profile-'));
const home = join(scratch, 'home');
const profile = join(home, 'profiles', 'opensandbox');
const env = {
  PATH: process.env.PATH, HOME: scratch, USERPROFILE: scratch,
  ...Object.fromEntries(['HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY', 'http_proxy', 'https_proxy', 'all_proxy', 'no_proxy',
    'NODE_EXTRA_CA_CERTS', 'NODE_USE_ENV_PROXY', 'SSL_CERT_FILE', 'NPM_CONFIG_PROXY', 'NPM_CONFIG_HTTPS_PROXY',
    'NPM_CONFIG_HTTP_PROXY', 'NPM_CONFIG_NOPROXY', 'npm_config_proxy', 'npm_config_https_proxy', 'npm_config_http_proxy', 'npm_config_noproxy']
    .filter(name => process.env[name] !== undefined).map(name => [name, process.env[name]])),
  DSH_HOME: home, XDG_CACHE_HOME: join(scratch, 'cache'), XDG_DATA_HOME: join(scratch, 'data'), XDG_STATE_HOME: join(scratch, 'state'),
  CI: '1', NO_COLOR: '1', npm_config_cache: join(tmpdir(), 'opensandbox-native-npm-cache'),
  OPENSANDBOX_DOMAIN: 'offline.invalid:443', OPENSANDBOX_TEMPLATE_ID: 'offline-template',
};
const run = async (label, args, expected = 0, extras = {}) => {
  const result = spawnSync(process.execPath, [cli, ...args], {cwd: scratch, env: {...env, ...extras}, encoding: 'utf8', timeout: 180_000, maxBuffer: 16 * 1024 * 1024});
  const output = `${result.stdout ?? ''}\n${result.stderr ?? ''}`.replace(/\?token=[^\s]+/g, '?token=[redacted]');
  await writeFile(join(scratch, `${label}.log`), output);
  assert.equal(result.error, undefined, `${label}: ${result.error?.message}\n${output}`);
  assert.equal(result.status, expected, `${label} exited ${result.status}; see ${join(scratch, `${label}.log`)}:\n${output.slice(-16_000)}`);
  return output;
};
let success = false;
try {
  const tarball = await pack(scratch);
  // This is the actual supported default command: no forced peers, no ignore scripts,
  // no extra dependency installs and no consumer overrides.
  await run('init', ['--profile', 'opensandbox', '--from-default-profile', 'web', '--dump-config']);
  await cp(join(root, 'examples/opensandbox-web.patch.yml'), join(profile, 'cordis.patch.yml'));
  await run('plugin-add', ['plugin', '--profile', 'opensandbox', 'add', tarball]);
  const manifestPath = join(profile, 'package.json');
  const profileManifest = JSON.parse(await readFile(manifestPath, 'utf8'));
  assert(profileManifest.dsh.profile.bundles.includes('@opensandbox/deepseek-harness-native'));
  assert.equal(profileManifest.overrides, undefined);
  assert.equal(profileManifest.pnpm?.overrides, undefined);
  const workspace = parse(await readFile(join(profile, 'pnpm-workspace.yaml'), 'utf8'));
  assert.equal(workspace.autoInstallPeers, false, 'Official CLI must provide its peers');
  const installed = join(profile, 'node_modules', '@opensandbox/deepseek-harness-native');
  const installedManifest = JSON.parse(await readFile(join(installed, 'package.json'), 'utf8'));
  assert.equal(installedManifest.version, '0.1.0');
  await assert.rejects(readFile(join(profile, 'node_modules', '@opensandbox/deepseek-harness/package.json')));
  // A profile-local host copy would defeat the ordinary host-peer identity contract.
  const deepseek = await readdir(join(profile, 'node_modules', '@deepseek-ai')).catch(() => []);
  assert.deepEqual(deepseek, [], 'Unexpected profile-local DeepSeek host copies');
  assert.equal(createHash('sha256').update(await readFile(join(installed, 'dist/remote/fs_helper.py'))).digest('hex'),
    createHash('sha256').update(await readFile(join(root, '../remote/fs_helper.py'))).digest('hex'));
  // This alpha's schema dumper rejects even disabled stock preset carrier classes.
  // Check the installed entry's schema and allow only those four upstream diagnostics.
  const schema = await run('schema', ['--profile', 'opensandbox', '--dump-config-schema'], 1);
  const schemaDocument = JSON.parse(schema.slice(schema.indexOf('{'), schema.lastIndexOf('}') + 1));
  const rows = schemaDocument['x-cordis'].entries;
  const nativeSchema = rows.find(row => row.id === 'opensandbox-native');
  assert.equal(nativeSchema?.status, 'schema');
  assert.match(JSON.stringify(schemaDocument.$defs[nativeSchema.configRef.split('/').pop()]), /secret/);
  const errors = schemaDocument['x-cordis'].diagnostics.filter(diagnostic => diagnostic.level === 'error');
  assert.deepEqual(errors.map(error => rows.find(row => row.path === error.path)?.id).sort(),
    ['preset-standard', 'preset-ptc', 'preset-minimal', 'preset-cordis'].sort());
  assert(errors.every(error => error.message.startsWith('unrecognized Loader tree carrier;')));
  const probe = join(scratch, 'probe.mjs');
  await cp(join(root, 'scripts/profile-probe.mjs'), probe);
  // Reuse the established offline transport fixtures without importing Vitest into
  // the native CLI. The generated stub implements only their three mock methods.
  const fixtures = join(scratch, 'fixtures');
  await mkdir(fixtures);
  await writeFile(join(fixtures, 'offline-mocks.mjs'), `export const vi = {fn(implementation = () => undefined) {
    const call = (...args) => { call.mock.calls.push(args); return implementation(...args); };
    call.mock = {calls: []}; call.mockImplementation = value => { implementation = value; return call; };
    call.mockResolvedValue = value => call.mockImplementation(async () => value); return call;
  }};\n`);
  for (const name of ['helper-sdk', 'sdk-facade', 'execd-lines']) {
    const source = (await readFile(join(root, `../tests/fixtures/${name}.ts`), 'utf8'))
      .replace("from 'vitest'", "from './offline-mocks.mjs'");
    await writeFile(join(fixtures, `${name}.js`), ts.transpileModule(source, {
      compilerOptions: {target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022},
    }).outputText);
  }
  await writeFile(join(fixtures, 'package.json'), '{"type":"module"}\n');
  const offline = join(scratch, 'offline.patch.yml');
  // Disable only external telemetry/account/model transport in this offline test.
  // SessionController, AgentLoop, persistence, registry, and Web composition remain real.
  const noNetwork = ['session-telemetry-otel', 'desktop-product-telemetry', 'product-analytics',
    'deepseek-account', 'account-controller', 'llm-deepseek', 'llm-deepseek-account', 'llm-pi-ai', 'session-title-llm'];
  const offlineConfig = noNetwork.map(id => `- id: ${id}\n  disabled: true\n`).join('') +
    '- id: agent-default-model\n  config:\n    provider: offline-existing-provider\n    model: offline-existing-model\n' +
    '- id: opensandbox-native\n  config:\n    domain: !!js process.env.OPENSANDBOX_DOMAIN\n    apiKey: !!js process.env.OPENSANDBOX_API_KEY\n    protocol: https\n    kind: create-template\n    templateId: !!js process.env.OPENSANDBOX_TEMPLATE_ID\n    remoteCwd: /workspace\n';
  await writeFile(offline, offlineConfig +
    `- insert:\n  - id: offline-native-probe\n    name: ${JSON.stringify(pathToFileURL(probe).href)}\n`);
  // Negative startup must fail because real required native rows are gated, not
  // because a test probe is waiting for the same services.
  const gateOnly = join(scratch, 'gate-only.patch.yml');
  await writeFile(gateOnly, offlineConfig);
  const guard = join(scratch, 'network-guard.mjs');
  await writeFile(guard, `globalThis.fetch = async () => { throw new Error('Offline test: external fetch is forbidden'); };\n`);
  const args = ['--profile', 'opensandbox', '--patch', offline, '--no-open', '--port', '0'];
  const gateArgs = ['--profile', 'opensandbox', '--patch', gateOnly, '--no-open', '--port', '0'];
  const assertGate = output => {
    assert.match(output, /agent-loop \(required\)[^\n]*opensandboxNativeReady/);
    assert.match(output, /webserver \(required\)[^\n]*opensandboxNativeReady/);
    assert.doesNotMatch(output, /offline-native-probe|NATIVE_PROFILE_PASS|dsh web: http/);
  };
  const extras = {NATIVE_TEST_ARTIFACT: installed, NATIVE_TEST_FIXTURES: fixtures, NATIVE_TEST_CWD: join(scratch, 'workspace'), NODE_OPTIONS: `--import=${pathToFileURL(guard).href}`};
  await mkdir(extras.NATIVE_TEST_CWD);
  assert.match(await run('web-create', args, 0, {...extras, NATIVE_TEST_MODE: 'create'}), /NATIVE_PROFILE_PASS create/);
  assert.match(await run('web-resume', args, 0, {...extras, NATIVE_TEST_MODE: 'resume'}), /NATIVE_PROFILE_PASS resume/);
  console.log('actual CLI default-entry remote tools/helper, Web create/cold resume, model two-turn reuse, session isolation, singleflight, SDK failures, cleanup and host identities: PASS (offline fixtures)');

  // Keep the exact dedicated profile fence while removing bundle activation.
  profileManifest.dsh.profile.bundles = profileManifest.dsh.profile.bundles.filter(name => name !== '@opensandbox/deepseek-harness-native');
  await writeFile(manifestPath, JSON.stringify(profileManifest, null, 2));
  const absent = await run('bundle-absent', gateArgs, 1, extras);
  assertGate(absent);
  profileManifest.dsh.profile.bundles.push('@opensandbox/deepseek-harness-native');
  await writeFile(manifestPath, JSON.stringify(profileManifest, null, 2));
  const disabled = join(scratch, 'disabled.patch.yml');
  await writeFile(disabled, '- id: opensandbox-native\n  disabled: true\n');
  assertGate(await run('native-disabled', ['--profile', 'opensandbox', '--patch', gateOnly, '--patch', disabled, '--no-open', '--port', '0'], 1, extras));
  const invalid = await run('invalid-config', gateArgs, 1, {...extras, OPENSANDBOX_DOMAIN: '', OPENSANDBOX_TEMPLATE_ID: ''});
  assertGate(invalid);
  const missingRegistry = join(scratch, 'missing-registry.patch.yml');
  await writeFile(missingRegistry, '- id: agent-preset-registry\n  disabled: true\n');
  const missing = await run('missing-registry', ['--profile', 'opensandbox', '--patch', gateOnly, '--patch', missingRegistry, '--no-open', '--port', '0'], 1, extras);
  assertGate(missing);
  assert.match(missing, /agent-loop \(required\)[^\n]*agentPresets/);
  console.log('persistent required-service fence: bundle absent, native disabled, invalid configuration, missing registry: PASS');
  success = true;
} finally {
  if (success) await rm(scratch, {recursive: true, force: true});
  else console.error(`Native profile diagnostic logs retained at ${scratch}`);
}
