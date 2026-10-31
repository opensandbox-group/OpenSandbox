// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

// Start the ordinary CLI with its installed tarball for real browser acceptance.
// No browser API is called here. A browser driver or human must create the session
// and submit both prompts through the normal visible composer.
import assert from 'node:assert/strict';
import { spawn, spawnSync } from 'node:child_process';
import { createWriteStream } from 'node:fs';
import { chmod, cp, mkdir, mkdtemp, readFile, writeFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import ts from 'typescript';
import { pack } from './pack-check.mjs';
import { root } from './stage.mjs';
import { browserPrompts } from './browser-ui.mjs';

const redact = text => String(text).replace(/([?&]token=)[^\s"'<>]+/g, '$1[redacted]');

export { browserPrompts };

export async function startBrowserFixture(options = {}) {
  const scratch = options.scratch ? resolve(options.scratch) : await mkdtemp(join(tmpdir(), 'opensandbox-native-browser-'));
  await mkdir(scratch, {recursive: true});
  const cli = createRequire(import.meta.url).resolve('@deepseek-ai/dsh/lib/bin.js');
  const home = join(scratch, 'home'), profile = join(home, 'profiles', 'opensandbox');
  const evidencePath = join(scratch, 'evidence.json');
  // Do not leak the caller's model keys, account tokens or real sandbox settings
  // into a disposable browser acceptance process. Registry proxy/CA settings are
  // needed only for the ordinary CLI plugin installation.
  const passthrough = ['PATH', 'HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY',
    'http_proxy', 'https_proxy', 'all_proxy', 'no_proxy', 'NODE_EXTRA_CA_CERTS',
    'NODE_USE_ENV_PROXY', 'SSL_CERT_FILE', 'NPM_CONFIG_PROXY', 'NPM_CONFIG_HTTPS_PROXY',
    'NPM_CONFIG_HTTP_PROXY', 'NPM_CONFIG_NOPROXY', 'npm_config_proxy',
    'npm_config_https_proxy', 'npm_config_http_proxy', 'npm_config_noproxy'];
  const env = { ...Object.fromEntries(passthrough.filter(name => process.env[name] !== undefined).map(name => [name, process.env[name]])),
    HOME: scratch, USERPROFILE: scratch, DSH_HOME: home,
    XDG_CACHE_HOME: join(scratch, 'cache'), XDG_DATA_HOME: join(scratch, 'data'), XDG_STATE_HOME: join(scratch, 'state'),
    CI: '1', NO_COLOR: '1', npm_config_cache: join(tmpdir(), 'opensandbox-native-npm-cache'),
    OPENSANDBOX_DOMAIN: 'offline.invalid:443', OPENSANDBOX_TEMPLATE_ID: 'offline-template',
  };
  const run = async (label, args) => {
    const result = spawnSync(process.execPath, [cli, ...args], { cwd: scratch, env, encoding: 'utf8', timeout: 180_000, maxBuffer: 16 * 1024 * 1024 });
    const output = `${result.stdout ?? ''}\n${result.stderr ?? ''}`;
    await writeFile(join(scratch, `${label}.log`), redact(output));
    assert.equal(result.error, undefined, `${label}: ${result.error?.message}`);
    assert.equal(result.status, 0, `${label} failed: ${redact(output.slice(-16_000))}`);
  };
  assert(!options.reuseProfile || options.tarball, 'Reusing a profile requires its exact original tarball path');
  const tarball = options.tarball ? resolve(options.tarball) : await pack(scratch);
  if (!options.reuseProfile) {
    await run('init', ['--profile', 'opensandbox', '--from-default-profile', 'web', '--dump-config']);
    await cp(options.fence ?? join(root, 'examples/opensandbox-web.patch.yml'), join(profile, 'cordis.patch.yml'));
    await run('plugin-add', ['plugin', '--profile', 'opensandbox', 'add', tarball]);
  }
  const profileManifest = JSON.parse(await readFile(join(profile, 'package.json'), 'utf8'));
  assert(profileManifest.dsh.profile.bundles.includes('@opensandbox/deepseek-harness-native'));
  assert.equal(profileManifest.overrides, undefined);
  assert.equal(profileManifest.pnpm?.overrides, undefined);
  const artifact = join(profile, 'node_modules', '@opensandbox/deepseek-harness-native');
  const fixtures = join(scratch, 'fixtures');
  await mkdir(fixtures, {recursive: true});
  await writeFile(join(fixtures, 'offline-mocks.mjs'), `export const vi = {fn(implementation = () => undefined) {
    const call = (...args) => { call.mock.calls.push(args); return implementation(...args); };
    call.mock = {calls: []}; call.mockImplementation = value => { implementation = value; return call; };
    call.mockResolvedValue = value => call.mockImplementation(async () => value); return call;
  }};\n`);
  for (const name of ['helper-sdk', 'sdk-facade', 'execd-lines']) {
    const source = (await readFile(join(root, `../tests/fixtures/${name}.ts`), 'utf8')).replace("from 'vitest'", "from './offline-mocks.mjs'");
    await writeFile(join(fixtures, `${name}.js`), ts.transpileModule(source, {
      compilerOptions: {target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022},
    }).outputText);
  }
  await writeFile(join(fixtures, 'package.json'), '{"type":"module"}\n');
  await cp(join(root, 'scripts/browser-fixture.mjs'), join(scratch, 'browser-fixture.mjs'));
  const offline = join(scratch, 'offline.patch.yml');
  const noNetwork = ['session-telemetry-otel', 'desktop-product-telemetry', 'product-analytics',
    'deepseek-account', 'account-controller', 'llm-deepseek', 'llm-deepseek-account', 'llm-pi-ai', 'session-title-llm'];
  await writeFile(offline, noNetwork.map(id => `- id: ${id}\n  disabled: true\n`).join('') +
    '- id: agent-default-model\n  config:\n    provider: offline-existing-provider\n    model: offline-existing-model\n' +
    '- id: opensandbox-native\n  config:\n    domain: !!js process.env.OPENSANDBOX_DOMAIN\n    apiKey: !!js process.env.OPENSANDBOX_API_KEY\n    protocol: https\n    kind: create-template\n    templateId: !!js process.env.OPENSANDBOX_TEMPLATE_ID\n    remoteCwd: /workspace\n' +
    `- insert:\n  - id: offline-browser-fixture\n    name: ${JSON.stringify(pathToFileURL(join(scratch, 'browser-fixture.mjs')).href)}\n`);
  const guard = join(scratch, 'network-guard.mjs');
  await writeFile(guard, `globalThis.fetch = async () => { throw new Error('Offline browser test: external fetch is forbidden'); };\n`);
  const child = spawn(process.execPath, [cli, '--profile', 'opensandbox', '--patch', offline, '--no-open', '--port', String(options.port ?? 0)], {
    cwd: scratch, env: { ...env, NATIVE_TEST_ARTIFACT: artifact, NATIVE_TEST_FIXTURES: fixtures,
      NATIVE_BROWSER_EVIDENCE: evidencePath, NODE_OPTIONS: `--import=${pathToFileURL(guard).href}` },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const logPath = join(scratch, 'web.log');
  const log = createWriteStream(logPath);
  let output = '', logPending = '', closed = false, exitCode;
  const exited = new Promise(resolveExit => child.once('close', code => {
    closed = true; exitCode = code; log.end(redact(logPending)); resolveExit(code);
  }));
  const collect = bytes => {
    output += bytes.toString(); logPending += bytes.toString();
    const lines = logPending.split('\n'); logPending = lines.pop();
    if (lines.length) log.write(redact(`${lines.join('\n')}\n`));
  };
  child.stdout.on('data', collect); child.stderr.on('data', collect);
  const stop = async () => {
    if (!closed) {
      child.kill('SIGTERM');
      const timeout = setTimeout(() => child.kill('SIGKILL'), 10_000);
      await exited; clearTimeout(timeout);
    }
  };
  try {
    const deadline = Date.now() + 60_000;
    let url, evidence;
    while (Date.now() < deadline) {
      assert(!closed, `CLI exited ${exitCode}; see ${logPath}:\n${redact(output.slice(-16_000))}`);
      url = output.match(/dsh web:\s+(http[^\s\u001b]+)/)?.[1];
      evidence = await readFile(evidencePath, 'utf8').then(JSON.parse).catch(() => undefined);
      if (url && evidence?.fixtureReady && evidence.nativeReady) break;
      await new Promise(resolveWait => setTimeout(resolveWait, 100));
    }
    assert(url, `Web server did not start; see ${logPath}:\n${redact(output.slice(-16_000))}`);
    assert(evidence?.fixtureReady && evidence.nativeReady, `Offline native fixture did not become ready; see ${logPath}`);
    const metadata = { scratch, profile, tarball, url, pid: child.pid, evidencePath, logPath, prompts: browserPrompts };
    // Only the explicit manual launcher persists its token, in a private handoff
    // outside the repository. CI retains it in memory, never in exported logs.
    const handoff = join(scratch, 'browser.json');
    await writeFile(handoff, JSON.stringify(options.privateHandoff ? metadata : {...metadata, url: '[redacted]'}, null, 2), {mode: 0o600});
    await chmod(handoff, 0o600);
    return { ...metadata, child, exited, stop,
      evidence: () => readFile(evidencePath, 'utf8').then(JSON.parse) };
  } catch (error) { await stop(); throw error; }
}

export function assertBrowserEvidence(evidence) {
  assert.deepEqual(evidence.errors, []);
  assert(evidence.nativeReady);
  assert(Object.values(evidence.services).every(Boolean));
  assert.equal(evidence.workspaceMetadata.directory, evidence.workspaceMetadata.expectedDirectory);
  // Native session headers require positive numeric metadata. Empty mediaTypes
  // advertises no supported images; authoritative attachment methods deny them.
  assert.deepEqual(evidence.attachmentImageLimits, { mediaTypes: [], maxImageBytes: 1,
    maxImagesPerMessage: 1, maxMessageImageBytes: 1, maxImagePixels: 1, maxImageDimension: 1 });
  assert.deepEqual(evidence.selection, evidence.currentSelection);
  assert.equal(evidence.sdkCalls.length, 1);
  assert.equal(evidence.sdkCalls[0].kind, 'create-template');
  assert.equal(evidence.modelRequests.length, 6);
  assert.equal(evidence.modelRequests[0].sdkCallsBefore, 0, 'The first UI-created Agent stays lazy until its first file tool');
  assert.deepEqual(evidence.modelRequests.map(request => request.tool), ['write', null, 'read', 'edit', 'bash', null]);
  const id = evidence.modelRequests[0].sessionId;
  assert(evidence.modelRequests.every(request => request.sessionId === id && request.provider === evidence.selection.provider && request.model === evidence.selection.model));
  const session = evidence.sessions.find(session => session.id === id);
  assert(session);
  assert(session.sameAgent && session.sameOptions && session.sameSession && session.sameHeader);
  assert.equal(session.sdkCallsAtCreate, 0);
  assert.equal(session.header.agentPreset, 'opensandbox');
  assert.deepEqual(session.tools, ['bash', 'edit', 'opensandbox_delete', 'opensandbox_reconnect', 'opensandbox_status', 'read', 'write']);
  const results = session.events.filter(event => event.type === 'tool/result');
  assert.equal(results.length, 4);
  assert(results.every(event => !event.data.message.isError));
  // The native loop also records its runtime-context snapshot as a user-role
  // message. Count only the human prompts submitted through the browser.
  const prompts = session.events.filter(event => event.type === 'user/message' && event.data.source.kind === 'user');
  assert.equal(prompts.length, 2);
  assert.deepEqual(prompts.map(event => event.data.content.filter(part => part.type === 'text').map(part => part.text).join('')), browserPrompts);
  assert.equal(evidence.sandboxes[0].note, 'second browser turn\n');
  assert(evidence.sandboxes[0].helperRequests > 0);
  assert(evidence.sandboxes[0].preflightCalls > 0);
  assert.deepEqual(evidence.denials, { hostResolve: true, hostPathUnmapped: true, upload: true,
    uploadStream: true, fileReceipt: true, textOnlyPromptAllowed: true, noHostExecutionServices: true });
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const argv = process.argv.slice(2);
  const value = flag => argv.includes(flag) ? argv[argv.indexOf(flag) + 1] : undefined;
  if (argv.includes('--assert')) {
    assertBrowserEvidence(JSON.parse(await readFile(value('--assert'), 'utf8')));
    console.log('NATIVE_BROWSER_EVIDENCE_PASS UI-two-turn native-identity lazy-file-first sdk-reuse host-denied upload-denied');
  } else {
    assert(argv.includes('--serve'), 'Use --serve [--scratch PATH] [--tarball PATH] [--fence PATH] [--port NUMBER] [--reuse-profile], or --assert EVIDENCE');
    const fixture = await startBrowserFixture({ scratch: value('--scratch'), tarball: value('--tarball'), fence: value('--fence'), port: value('--port'), reuseProfile: argv.includes('--reuse-profile'), privateHandoff: true });
    console.log(JSON.stringify({ privateHandoff: join(fixture.scratch, 'browser.json'), scratch: fixture.scratch, evidencePath: fixture.evidencePath, prompts: browserPrompts }, null, 2));
    process.once('SIGTERM', () => { void fixture.stop(); });
    process.once('SIGINT', () => { void fixture.stop(); });
    await fixture.exited;
  }
}
