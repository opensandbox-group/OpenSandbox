// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

// OFFLINE regression tests: fake SDK network and session boundaries, actual plugin opener/shell.
// These checks do not establish a deployed Ubuntu result.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync, rmSync, existsSync, readdirSync, symlinkSync } from 'node:fs';
import { join, basename } from 'node:path';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { Context } from '@deepseek-ai/cordis';
import { SandboxApiException } from '@alibaba-group/opensandbox';
import type { Sandbox, SandboxCreateFromTemplateOptions, SandboxConnectOptions } from '@alibaba-group/opensandbox';
import type { GenerateOptions, LlmAdapter } from '@deepseek-ai/dsh-llm';
import type { BoundSandbox, HeadlessSession, SdkFacade, OpenOptions } from '@opensandbox/deepseek-harness';

const boundary = vi.hoisted(() => ({create: vi.fn(), connect: vi.fn(), manager: vi.fn(), session: vi.fn(), openCalls: [] as {kind: string}[],
  failCreateRoute: false, failBindingKill: false, ignorePluginAbort: false, crossRead: false, hostLeak: false, inaccessibleHost: false}));
vi.mock('node:fs', async original => {
  const actual = await original<typeof import('node:fs')>();
  return {...actual, lstatSync: (...args: Parameters<typeof actual.lstatSync>) => {
    if (boundary.inaccessibleHost && String(args[0]).includes('dsh-e2e-')) throw Object.assign(new Error('private permission detail'), {code: 'EACCES'});
    return actual.lstatSync(...args);
  }};
});
vi.mock('@alibaba-group/opensandbox', async original => ({...await original<typeof import('@alibaba-group/opensandbox')>(),
  Sandbox: {createFromTemplate: boundary.create, connect: boundary.connect}, SandboxManager: {create: boundary.manager}}));
vi.mock('@opensandbox/deepseek-harness', async original => {
  const actual = await original<typeof import('@opensandbox/deepseek-harness')>();
  return {...actual, createHeadlessSession: boundary.session, openBinding: actual.createBindingOpener({create: options => boundary.create(options), createFromTemplate: options => boundary.create(options), connect: options => boundary.connect(options)}), createBindingOpener: (sdk: SdkFacade) => {
    const open = actual.createBindingOpener(sdk);
    return async (options: OpenOptions) => {
      boundary.openCalls.push(options);
      if (options.kind === 'create-template' && boundary.failCreateRoute) throw new Error('plugin route failed');
      const binding = await open(options);
      if (!boundary.failBindingKill) return binding;
      return {descriptor: binding.descriptor, sandbox: binding.sandbox, get state() {return binding.state;},
        close: () => binding.close(), kill: async () => {throw new Error('plugin kill path failed');}, toJSON: () => binding.toJSON()};
    };
  }};
});
import { RemoteShell } from '@opensandbox/deepseek-harness';
import { main, readE2eConfig, runE2e, writeEvidence } from '../e2e.js';
const roots: string[] = [];
function fixture() {
  const root = mkdtempSync(join(tmpdir(), 'dsh-review-offline-')); roots.push(root);
  for (const dir of ['env', 'evidence', 'downloads', 'host-observation']) mkdirSync(join(root, dir));
  const env = {DSH_LIVE_E2E: '1', OPEN_SANDBOX_DOMAIN: 'example.invalid:18080', OPEN_SANDBOX_PROTOCOL: 'http',
    OPEN_SANDBOX_API_KEY: 'secret-key-do-not-publish', DSH_TEMPLATE_ID: 'offline-template', DSH_SESSION_ID: 'offline-review',
    DSH_REMOTE_CWD: '/srv/app', DSH_TTL_SECONDS: '600', WORK: join(root, 'env'), DSH_EVIDENCE_PATH: join(root, 'evidence', 'plugin.json'),
    DSH_DOWNLOADS_DIR: join(root, 'downloads'), DSH_HOST_OBSERVATION_DIR: join(root, 'host-observation'),
    DSH_WORKSPACE_ROOT: root, XFS_MOUNT_POINT: join(root, 'env', 'stateroot'), FSB_DIR: join(root, 'env', 'fast-sandbox'),
    XFS_LOOP_FILE: join(root, 'env', 'fast-sandbox.img'), KIND_CLUSTER: 'offline-cluster', RUSTFS_CONTAINER: 'offline-rustfs'};
  return {root, env, config: () => readE2eConfig(env)};
}
function deferred<T>() {let resolve!: (value: T) => void; const promise = new Promise<T>(done => {resolve = done;}); return {promise, resolve};}
function fakeNetwork(f: ReturnType<typeof fixture>) {
  const controls = {unknownDelete: false, failReadiness: false, failPreflight: false, failInterrupt: false, hangInterrupt: false, unknownStatus: false};
  const items: {id: string; data: Map<string, Uint8Array>; deleted: boolean; kill: ReturnType<typeof vi.fn>; ready: ReturnType<typeof vi.fn>}[] = [];
  const status = vi.fn(async (id: string) => controls.unknownStatus ? {id} : {id, running: false, exitCode: 143});
  const interrupt = vi.fn(async (_id: string) => {
    if (controls.failInterrupt) throw new Error('private interrupt error');
    if (controls.hangInterrupt) await new Promise(() => undefined);
  });
  const sdk = (item: typeof items[number]) => ({id: item.id, waitUntilReady: item.ready, kill: item.kill, close: vi.fn(async () => undefined),
    files: {writeFiles: vi.fn(async (entries: {path: string; data: Uint8Array}[]) => {for (const e of entries) item.data.set(e.path, e.data);}),
      readBytes: vi.fn(async (path: string) => new Uint8Array(item.data.get(path)!)),
      readBytesStream: async function*(path: string) {yield new Uint8Array(item.data.get(path)!);}, deleteDirectories: vi.fn(async () => undefined)},
    commands: {interrupt, getCommandStatus: status,
      run: vi.fn(async (_command: unknown, _options: unknown, handlers?: {onInit?: (v: {id: string}) => void}, signal?: AbortSignal) => {
        if (controls.failPreflight) throw new Error('private prereq error');
        handlers?.onInit?.({id: 'offline-command'}); if (signal?.aborted) throw new Error('observation abort');
        return {complete: true, exitCode: 0};
      }), runStream: async function*() {yield {type: 'init', text: 'offline-command'}; await new Promise(() => undefined);},
    }}) as unknown as Sandbox;
  boundary.create.mockImplementation(async (_opts: SandboxCreateFromTemplateOptions) => {
    const item = {id: `offline-${items.length + 1}`, data: new Map<string, Uint8Array>(), deleted: false,
      ready: vi.fn(async () => {if (controls.failReadiness) throw new Error('private readiness error');}),
      kill: vi.fn(async () => {if (controls.unknownDelete) throw new Error('unknown DELETE outcome'); item.deleted = true;})};
    items.push(item); return sdk(item);
  });
  boundary.connect.mockImplementation(async (opts: SandboxConnectOptions) => sdk(items.find(item => item.id === opts.sandboxId)!));
  const manager = {killSandbox: vi.fn(async (id: string) => {items.find(item => item.id === id)!.deleted = true;}),
    getSandboxInfo: vi.fn(async (id: string) => {
      const item = items.find(value => value.id === id)!;
      if (item.deleted || controls.unknownDelete) throw new SandboxApiException({statusCode: 404, message: 'private diagnostics'});
      return {id};
    }), close: vi.fn(async () => undefined)};
  boundary.manager.mockReturnValue(manager);
  boundary.session.mockImplementation(async ({binding, adapter}: {binding: BoundSandbox; adapter: LlmAdapter}) => {
    const item = items.find(s => s.id === binding.descriptor.sandboxId)!, ctx = new Context();
    const shell = new RemoteShell(ctx, {binding, settlementTimeoutMs: 100});
    const execute = shell.execute;
    const publicShell = {resolve: shell.resolve, execute: (spec: Parameters<typeof execute>[0]) => execute(boundary.ignorePluginAbort ? {...spec, signal: undefined} : spec)};
    const events: unknown[] = [], listeners = new Map<string, (...args: unknown[]) => void>();
    return {agent: {ctx: {root: {shell: publicShell}, on: (name: string, fn: (...args: unknown[]) => void) => {listeners.set(name, fn); return () => undefined;}},
      session: {snapshotEvents: () => events}}, dispose: vi.fn(async () => {await shell.dispose();}), run: async () => {
      for (;;) {
        const chunks = []; for await (const chunk of adapter.stream({} as GenerateOptions)) chunks.push(chunk);
        const chunk = chunks.find(c => c.type === 'block-end');
        if (chunk?.type !== 'block-end' || chunk.block.type !== 'tool-call') break;
        const block = chunk.block, args = JSON.parse(block.arguments), name = block.name;
        let path = `${binding.descriptor.remoteCwd}/${args.file_path ?? 'note.txt'}`, value: unknown = {kind: 'foreground', exitCode: 0, timedOut: false};
        if (name === 'bash') {
          const parsed = /printf '%s' '([^']+)' > '?([^'\s]+)'?/.exec(args.command); if (!parsed) throw new Error('Unexpected fake-network script');
          path = `${binding.descriptor.remoteCwd}/${parsed[2]}`; item.data.set(path, Buffer.from(parsed[1]!));
          if (boundary.hostLeak) writeFileSync(join(f.env.DSH_HOST_OBSERVATION_DIR, basename(path)), 'unexpected host artifact');
        } else if (name === 'edit') item.data.set(path, Buffer.from(Buffer.from(item.data.get(path)!).toString().replace(args.old_string, args.new_string)));
        else if (name === 'read') {
          let text = Buffer.from(item.data.get(path)!).toString();
          if (boundary.crossRead && binding.descriptor.sessionId.includes('reconnected')) text = text.replace('primary', 'secondary');
          value = {path, offset: 1, totalLines: 1, lines: [{number: 1, text}]};
        }
        const content = name === 'read' ? [{type: 'text', text: `<path>${path}</path>\n<type>file</type>\n<content>\n1: ${(value as {lines: {text: string}[]}).lines[0]!.text}\n\n(End of file - total 1 lines)\n</content>`}] : [{type: 'text', text: 'done'}];
        events.push({type: 'tool/result', data: {message: {toolCallId: block.id, isError: false, content}}});
        listeners.get('tools/result')?.({name, callId: block.id}, {isError: false, value, content});
      }
      events.push({type: 'turn/end', data: {reason: {kind: 'completed'}}});
    }} as unknown as HeadlessSession;
  });
  return {items, manager, controls, status, interrupt};
}
afterEach(() => {vi.useRealTimers(); vi.unstubAllEnvs(); vi.restoreAllMocks(); vi.clearAllMocks(); boundary.openCalls.length = 0;
  boundary.failCreateRoute = boundary.failBindingKill = boundary.ignorePluginAbort = boundary.crossRead = boundary.hostLeak = boundary.inaccessibleHost = false;
  for (const root of roots.splice(0)) rmSync(root, {recursive: true, force: true});});

describe('OFFLINE review regressions R1-R6', () => {
  it('traverses actual plugin create-template and binding.kill with healthy raw SDK', async () => {
    const f = fixture(), n = fakeNetwork(f), e = await runE2e(f.config()); expect(e.success).toBe(true);
    expect(boundary.openCalls.filter(c => c.kind === 'create-template')).toHaveLength(2);
    expect(n.items.every(item => item.kill.mock.calls.length === 1)).toBe(true); expect(n.manager.killSandbox).not.toHaveBeenCalled();
  });
  it('fails when the plugin create-template route fails while the raw SDK remains healthy', async () => {
    const f = fixture(); fakeNetwork(f); boundary.failCreateRoute = true;
    expect((await runE2e(f.config())).success).toBe(false); expect(boundary.create).not.toHaveBeenCalled();
  });
  it('fails a broken plugin binding.kill while raw SDK deletion remains healthy, without rescue', async () => {
    const f = fixture(), n = fakeNetwork(f); boundary.failBindingKill = true;
    n.manager.getSandboxInfo.mockRejectedValue(new SandboxApiException({statusCode: 404}));
    const e = await runE2e(f.config()); expect(e.success).toBe(false);
    expect(n.items.every(item => item.kill.mock.calls.length === 0)).toBe(true);
    expect(n.manager.killSandbox).not.toHaveBeenCalled(); expect(e.cleanup.sandboxes.every(item => item.sdkDeleteObserved === false)).toBe(true);
  });
  it('never issues a manager DELETE after an unknown plugin kill, even if GET later returns 404', async () => {
    const f = fixture(), n = fakeNetwork(f); n.controls.unknownDelete = true; const e = await runE2e(f.config());
    expect(e.success).toBe(false); expect(n.items.every(item => item.kill.mock.calls.length === 1)).toBe(true);
    expect(n.manager.killSandbox).not.toHaveBeenCalled(); expect(n.manager.getSandboxInfo).toHaveBeenCalledTimes(2);
  });
  it('retains opener cleanup ownership and never retries an unknown readiness-failure DELETE', async () => {
    const f = fixture(), n = fakeNetwork(f); n.controls.failReadiness = n.controls.unknownDelete = true;
    const e = await runE2e(f.config()); expect(e.success).toBe(false); expect(n.items[0]!.kill).toHaveBeenCalledTimes(1);
    expect(n.manager.killSandbox).not.toHaveBeenCalled(); expect(n.manager.getSandboxInfo).toHaveBeenCalledTimes(1);
  });
  it('fails if plugin AbortSignal behavior is removed while the SDK remains healthy', async () => {
    const f = fixture(); fakeNetwork(f); boundary.ignorePluginAbort = true; vi.useFakeTimers();
    const pending = runE2e(f.config()); await vi.runAllTimersAsync(); const e = await pending;
    expect(e.success).toBe(false); expect(e.stages.find(s => s.name === 'cancellation_status')?.status).toBe('failed');
  });
  it('observes status independently when plugin interrupt fails or is unresponsive', async () => {
    for (const flag of ['failInterrupt', 'hangInterrupt'] as const) {
      const f = fixture(), n = fakeNetwork(f); n.controls[flag] = true;
      const e = await runE2e(f.config()); expect(n.status).toHaveBeenCalled();
      expect(e.stages.find(s => s.name === 'cancellation_status')?.facts).toMatchObject({commandId: 'offline-command', running: false, aborted: true, timedOut: false});
    }
  });
  it('rejects a cross-routed successful primary read while both remote files remain correct', async () => {
    const f = fixture(); fakeNetwork(f); boundary.crossRead = true; const e = await runE2e(f.config());
    expect(e.success).toBe(false); expect(e.stages.find(s => s.name === 'session_isolation')?.status).toBe('failed');
  });
  it('rejects and retains a forbidden host artifact while remote operations remain correct', async () => {
    const f = fixture(); fakeNetwork(f); boundary.hostLeak = true; const e = await runE2e(f.config());
    expect(e.success).toBe(false); expect(e.stages.find(s => s.name === 'host_nonappearance_after')?.status).toBe('failed');
    expect(readdirSync(f.env.DSH_HOST_OBSERVATION_DIR).length).toBeGreaterThan(0);
  });
  it('checks host nonappearance after disposal and retains a late cleanup-time leak', async () => {
    const f = fixture(), n = fakeNetwork(f), original = boundary.session.getMockImplementation()!;
    boundary.session.mockImplementation(async (...args: unknown[]) => {
      const value = await original(...args) as HeadlessSession; let calls = 0;
      return {...value, dispose: async () => {
        await value.dispose();
        if (++calls > 1) {
          const path = [...n.items[0]!.data.keys()].find(path => path.endsWith('.note.txt'))!;
          writeFileSync(join(f.env.DSH_HOST_OBSERVATION_DIR, basename(path)), 'late local leak');
        }
      }};
    });
    const e = await runE2e(f.config()); expect(e.success).toBe(false);
    expect(e.stages.find(stage => stage.name === 'host_nonappearance_after')?.status).toBe('failed');
    expect(readdirSync(f.env.DSH_HOST_OBSERVATION_DIR).length).toBeGreaterThan(0);
  });
  it('fails closed on an inaccessible required host path before any sandbox creation', async () => {
    const f = fixture(); fakeNetwork(f); boundary.inaccessibleHost = true; const e = await runE2e(f.config());
    expect(e.success).toBe(false); expect(e.stages.find(stage => stage.name === 'host_nonappearance_before')?.status).toBe('failed');
    expect(boundary.create).not.toHaveBeenCalled(); expect(JSON.stringify(e)).not.toContain('private permission detail');
  });
  it('refuses a Corepack-style pnpm shim without invoking it or creating cache directories', () => {
    const f = fixture(), bin = join(f.root, 'bin'), cache = join(f.root, 'uncached-corepack'); mkdirSync(bin);
    writeFileSync(join(bin, 'pnpm'), `#!/bin/sh\n# Corepack download-capable shim\nmkdir -p '${cache}/v1'\necho 11.7.0\n`, {mode: 0o755});
    writeFileSync(join(f.root, 'package.json'), JSON.stringify({packageManager: 'pnpm@11.7.0'}));
    const before = readFileSync(join(f.root, 'package.json'), 'utf8');
    const result = spawnSync('bash', [new URL('../scripts/preflight-ubuntu.sh', import.meta.url).pathname], {cwd: f.root,
      env: {...process.env, ...f.env, DSH_PNPM_EXECUTABLE: '', PATH: `${bin}:${process.env.PATH}`, COREPACK_HOME: cache}, encoding: 'utf8', timeout: 15_000});
    expect(result.stdout).toContain('unresolved pnpm'); expect(existsSync(cache)).toBe(false);
    expect(readFileSync(join(f.root, 'package.json'), 'utf8')).toBe(before);
  });
  it('rejects the actual official shared XFS default and accepts the explicit dedicated path', () => {
    const f = fixture(), path = new URL('../scripts/preflight-ubuntu.sh', import.meta.url).pathname;
    const official = readFileSync(new URL('../../../scripts/fast-sandbox-env/integration-env.sh', import.meta.url), 'utf8');
    expect(official).toContain('XFS_MOUNT_POINT="${XFS_MOUNT_POINT:-/var/lib/fast-sandbox}"');
    expect(official).toContain('KIND_CLUSTER="${KIND_CLUSTER:-fast-sandbox-integration}"');
    expect(official).toContain('RUSTFS_CONTAINER="${RUSTFS_CONTAINER:-fast-sandbox-env-rustfs}"');
    const unsafe = spawnSync('bash', [path], {env: {...process.env, ...f.env, XFS_MOUNT_POINT: '/var/lib/fast-sandbox'}, encoding: 'utf8', timeout: 15_000});
    expect(unsafe.stdout).toContain('XFS_MOUNT_POINT must stay inside dedicated WORK');
    const dedicated = spawnSync('bash', [path], {env: {...process.env, ...f.env}, encoding: 'utf8', timeout: 15_000});
    expect(dedicated.stdout).not.toContain('XFS_MOUNT_POINT must');
  });
  it('requires explicit safety overrides instead of silently checking invented defaults', () => {
    const f = fixture(); const env = {...process.env, ...f.env};
    delete (env as NodeJS.ProcessEnv).XFS_MOUNT_POINT; delete (env as NodeJS.ProcessEnv).KIND_CLUSTER; delete (env as NodeJS.ProcessEnv).RUSTFS_CONTAINER;
    const result = spawnSync('bash', [new URL('../scripts/preflight-ubuntu.sh', import.meta.url).pathname], {env, encoding: 'utf8', timeout: 15_000});
    for (const key of ['XFS_MOUNT_POINT', 'KIND_CLUSTER', 'RUSTFS_CONTAINER']) expect(result.stdout).toContain(`${key} must be explicit`);
  });
});


describe('OFFLINE re-review regressions A/B', () => {
  for (const operation of ['kill', 'close'] as const) {
    it(`bounds unresponsive opener ${operation}, cleans the primary and never revives evidence on late resolution`, async () => {
      const f = fixture(), n = fakeNetwork(f), delayed = deferred<void>(), original = boundary.create.getMockImplementation()!;
      let observed: ReturnType<typeof vi.fn> | undefined;
      boundary.create.mockImplementation(async (options: SandboxCreateFromTemplateOptions) => {
        const sandbox = await original(options) as Sandbox;
        if (sandbox.id === 'offline-2') {
          sandbox.waitUntilReady = vi.fn(async () => {throw new Error('secondary preparation failed');});
          const raw = sandbox[operation].bind(sandbox);
          observed = vi.fn(async () => {await delayed.promise; await raw();}); sandbox[operation] = observed;
        }
        return sandbox;
      });
      // Independent GET evidence is available even while the DELETE acknowledgement is unknown.
      n.manager.getSandboxInfo.mockRejectedValue(new SandboxApiException({statusCode: 404}));
      vi.useFakeTimers(); let completed: Awaited<ReturnType<typeof runE2e>> | undefined;
      const pending = runE2e(f.config()).then(value => {completed = value; return value;});
      try {
        await vi.advanceTimersByTimeAsync(180_000);
        expect(completed, 'opener cleanup must release the independent finalizer within its observation budget').toBeDefined();
        const e = completed!;
        expect(e.success).toBe(false); expect(e.stages).toHaveLength(14);
        expect(e.stages.find(stage => stage.name === 'secondary_readiness')).toMatchObject({status: 'failed', facts: {sandboxId: 'offline-2'}});
        expect(n.items[0]!.kill).toHaveBeenCalledTimes(1); expect(observed).toHaveBeenCalledTimes(1);
        expect(n.manager.getSandboxInfo.mock.calls.map(([id]) => id)).toEqual(['offline-1', 'offline-2']);
        expect(n.manager.killSandbox).not.toHaveBeenCalled();
        expect(e.cleanup.sandboxes.find(item => item.sandboxId === 'offline-2')).toMatchObject({deletionAttempted: true,
          sdkDeleteObserved: true, killPath: 'opener-kill', kill: operation === 'kill' ? 'unconfirmed' : 'accepted',
          deleted: 'confirmed-404', localClosed: operation !== 'close'});
        const settledEvidence = JSON.stringify(e); delayed.resolve(); await vi.runAllTimersAsync();
        expect(JSON.stringify(e)).toBe(settledEvidence); expect(observed).toHaveBeenCalledTimes(1);
        expect(n.items.every(item => item.kill.mock.calls.length === 1)).toBe(true); expect(n.manager.killSandbox).not.toHaveBeenCalled();
      } finally {delayed.resolve(); await vi.runAllTimersAsync(); await pending;}
    });
  }
  it('preserves complete nonempty failed evidence for a duplicate secondary ID with one cleanup per unique ID', async () => {
    const f = fixture(), n = fakeNetwork(f), original = boundary.create.getMockImplementation()!;
    boundary.create.mockImplementation(async (options: SandboxCreateFromTemplateOptions) => {
      if (n.items.length) return boundary.connect({sandboxId: n.items[0]!.id});
      return original(options);
    });
    for (const [key, value] of Object.entries(f.env)) vi.stubEnv(key, value);
    const stdout = vi.spyOn(console, 'log').mockImplementation(() => undefined), exitCode = process.exitCode;
    try {
      await expect(main()).resolves.toBeUndefined();
      expect(process.exitCode).toBe(1);
      const text = readFileSync(f.env.DSH_EVIDENCE_PATH, 'utf8'); expect(text.length).toBeGreaterThan(0);
      const e = JSON.parse(text) as Awaited<ReturnType<typeof runE2e>>;
      expect(e).toMatchObject({schemaVersion: 2, success: false, cleanup: {creationOutcome: 'known'}}); expect(e.stages).toHaveLength(14);
      expect(e.stages.find(stage => stage.name === 'create_secondary')).toMatchObject({status: 'failed', facts: {sandboxId: 'offline-1', duplicateOwnedId: true}});
      expect(e.stages.find(stage => stage.name === 'secondary_readiness')?.status).toBe('skipped');
      expect(e.stages.find(stage => stage.name === 'secondary_preflight')?.status).toBe('skipped');
      expect(e.failures).toContainEqual({stage: 'create_secondary', code: 'duplicate_creation_id'});
      expect(e.cleanup.creationRequestIds).toHaveLength(2); expect(e.cleanup.sandboxes).toHaveLength(1);
      expect(e.cleanup.sandboxes[0]).toMatchObject({sandboxId: 'offline-1', deletionAttempted: true, sdkDeleteObserved: true, kill: 'accepted', deleted: 'confirmed-404'});
      expect(boundary.create).toHaveBeenCalledTimes(2); expect(n.items[0]!.kill).toHaveBeenCalledTimes(1);
      expect(n.manager.killSandbox).not.toHaveBeenCalled(); expect(n.manager.getSandboxInfo).toHaveBeenCalledTimes(1);
      expect(stdout).toHaveBeenCalledTimes(1);
    } finally {process.exitCode = exitCode;}
  });
});

describe('OFFLINE actual Agent/helper composition through the live entry', () => {
  it('validates real committed read formatting and plugin cancellation with a disposable fake SDK namespace', async () => {
    const f = fixture(), actual = await vi.importActual<typeof import('@opensandbox/deepseek-harness')>('@opensandbox/deepseek-harness');
    const {helperSdk} = await import('../../../integrations/deepseek-harness/tests/fixtures/helper-sdk.js');
    const namespaces: Awaited<ReturnType<typeof helperSdk>>[] = [], deleted = new Set<string>();
    const config = readE2eConfig({...f.env, DSH_REMOTE_CWD: '/workspace'});
    const clone = (value: typeof namespaces[number]) => ({...value.boundary, commands: {...value.commands},
      close: vi.fn(async () => undefined), kill: vi.fn(async () => {deleted.add(value.sandbox.id);})}) as unknown as Sandbox;
    boundary.create.mockImplementation(async () => {
      const value = await helperSdk(`offline-helper-${namespaces.length + 1}`, `offline-session-${namespaces.length + 1}`);
      value.files.readBytes.mockImplementation(async path => {
        const chunks: Uint8Array[] = []; for await (const chunk of value.files.readBytesStream(path)) chunks.push(chunk);
        return new Uint8Array(Buffer.concat(chunks));
      });
      namespaces.push(value); return clone(value);
    });
    boundary.connect.mockImplementation(async (options: SandboxConnectOptions) => clone(namespaces.find(value => value.sandbox.id === options.sandboxId)!));
    boundary.manager.mockReturnValue({killSandbox: vi.fn(async (id: string) => {deleted.add(id);}),
      getSandboxInfo: vi.fn(async (id: string) => {if (deleted.has(id)) throw new SandboxApiException({statusCode: 404}); return {id};}),
      close: async () => undefined});
    boundary.session.mockImplementation(actual.createHeadlessSession);
    const fetch = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new Error('Network forbidden in this OFFLINE contract test'));
    try {
      const e = await runE2e(config); expect(e.success).toBe(true);
      expect(e.stages.find(stage => stage.name === 'agent_tools')?.facts).toMatchObject({readValueVerified: true, committedReadVerified: true});
      expect(e.stages.find(stage => stage.name === 'cancellation_status')?.facts).toMatchObject({aborted: true, timedOut: false, launches: 1, running: false});
      expect(fetch).not.toHaveBeenCalled();
    } finally {for (const value of namespaces) await value.cleanup();}
  }, 40_000);
});

describe('OFFLINE entry guards, evidence and cleanup regressions', () => {
  it('requires each explicit live setting before any network call', () => {
    const f = fixture(); expect(() => readE2eConfig({})).toThrow('DSH_LIVE_E2E');
    for (const key of ['DSH_LIVE_E2E', 'OPEN_SANDBOX_DOMAIN', 'OPEN_SANDBOX_PROTOCOL', 'OPEN_SANDBOX_API_KEY', 'DSH_TEMPLATE_ID',
      'DSH_SESSION_ID', 'DSH_REMOTE_CWD', 'DSH_TTL_SECONDS', 'WORK', 'DSH_EVIDENCE_PATH', 'DSH_DOWNLOADS_DIR', 'DSH_HOST_OBSERVATION_DIR']) {
      expect(() => readE2eConfig({...f.env, [key]: undefined}), key).toThrow();
    }
    expect(() => readE2eConfig({...f.env, DSH_TTL_SECONDS: '0'})).toThrow();
    expect(() => readE2eConfig({...f.env, OPEN_SANDBOX_DOMAIN: 'https://wrong.invalid'})).toThrow();
    expect(boundary.create).not.toHaveBeenCalled();
  });
  it('refuses the CLI opt-in before network calls or evidence creation', () => {
    const result = spawnSync(process.execPath, ['--import', 'tsx', 'e2e.ts'], {
      cwd: new URL('../', import.meta.url), env: {PATH: process.env.PATH}, encoding: 'utf8', timeout: 10_000});
    expect(result.status).toBe(1); expect(result.stdout).toBe(''); expect(result.stderr).toContain('DSH_LIVE_E2E=1');
  });
  it('preserves an existing evidence file before any SDK call', async () => {
    const f = fixture(); writeFileSync(f.env.DSH_EVIDENCE_PATH, 'retained');
    for (const [key, value] of Object.entries(f.env)) vi.stubEnv(key, value);
    await expect(main()).rejects.toThrow(); expect(boundary.create).not.toHaveBeenCalled();
    expect(readFileSync(f.env.DSH_EVIDENCE_PATH, 'utf8')).toBe('retained');
  });
  it('keeps canonical evidence/download/host-observation paths outside teardown WORK', () => {
    const f = fixture();
    expect(() => readE2eConfig({...f.env, DSH_EVIDENCE_PATH: join(f.env.WORK, 'evidence.json')})).toThrow(/outside WORK/);
    expect(() => readE2eConfig({...f.env, DSH_DOWNLOADS_DIR: f.env.WORK})).toThrow(/outside WORK/);
    expect(() => readE2eConfig({...f.env, DSH_HOST_OBSERVATION_DIR: f.env.WORK})).toThrow(/outside WORK/);
    const alias = join(f.root, 'work-alias'); symlinkSync(f.env.WORK, alias);
    expect(() => readE2eConfig({...f.env, DSH_EVIDENCE_PATH: join(alias, 'evidence.json')})).toThrow(/outside WORK/);
  });
  it('records unknown SDK creation without retries or secret diagnostics', async () => {
    const f = fixture(); fakeNetwork(f); boundary.create.mockRejectedValue(new Error('secret-key-do-not-publish private headers'));
    const e = await runE2e(f.config()); expect(e.success).toBe(false); expect(boundary.create).toHaveBeenCalledTimes(1);
    expect(e.cleanup.creationOutcome).toBe('unknown'); expect(e.cleanup.sandboxes).toEqual([]);
    expect(e.cleanup.creationRequestIds).toHaveLength(1); expect(JSON.stringify(e)).not.toMatch(/secret-key|private headers/);
    const correlationId = boundary.create.mock.calls[0]![0].metadata['dsh-binding-request'];
    expect(correlationId).toEqual(expect.stringMatching(/^[0-9a-f-]{36}$/)); expect(e.cleanup.creationRequestIds).toEqual([correlationId]);
  });
  it('emits exclusive credential-free schema-v2 evidence with separate deletion observations', async () => {
    const f = fixture(), n = fakeNetwork(f), e = await runE2e(f.config()); expect(e.success).toBe(true);
    expect(e).toMatchObject({schemaVersion: 2, mode: 'real', failures: []}); expect(e.stages.every(stage => stage.status === 'passed')).toBe(true);
    expect(e.cleanup.sandboxes.every(item => item.killPath === 'binding-kill' && item.kill === 'accepted' && item.deleted === 'confirmed-404')).toBe(true);
    expect(n.manager.killSandbox).not.toHaveBeenCalled(); writeEvidence(f.config(), e);
    expect(readFileSync(f.env.DSH_EVIDENCE_PATH, 'utf8')).not.toMatch(/secret-key|primary-[0-9a-f]+-|Authorization|apiKey|headers|fileContents/);
    expect(() => writeEvidence(f.config(), e)).toThrow();
  });
  it('still deletes the known owned ID after failed Agent composition', async () => {
    const f = fixture(), n = fakeNetwork(f); boundary.session.mockRejectedValue(new Error('private cause'));
    const e = await runE2e(f.config()); expect(e.success).toBe(false); expect(n.items[0]!.kill).toHaveBeenCalledTimes(1);
    expect(e.stages.find(stage => stage.name === 'agent_tools')?.status).toBe('failed');
    expect(e.stages.find(stage => stage.name === 'download_hash')?.status).toBe('skipped');
    expect(e.stages.find(stage => stage.name === 'host_nonappearance_after')?.status).toBe('passed');
  });
  it('uses manager cleanup only when reconnect failed after close and no earlier DELETE was attempted', async () => {
    const f = fixture(), n = fakeNetwork(f); boundary.connect.mockRejectedValue(new Error('connect failed'));
    const e = await runE2e(f.config()); expect(e.success).toBe(false); expect(n.items[0]!.kill).not.toHaveBeenCalled();
    expect(n.manager.killSandbox).toHaveBeenCalledTimes(1);
    expect(e.cleanup.sandboxes[0]).toMatchObject({killPath: 'manager-cleanup', deletionAttempted: true, deleted: 'confirmed-404'});
  });
  it('bounds unresponsive disposal and still attempts each owned deletion independently', async () => {
    const f = fixture(), n = fakeNetwork(f), original = boundary.session.getMockImplementation()!;
    boundary.session.mockImplementation(async (...args: unknown[]) => {
      const value = await original(...args) as HeadlessSession; let calls = 0;
      return {...value, dispose: async () => {if (++calls > 1) await new Promise(() => undefined); else await value.dispose();}};
    });
    vi.useFakeTimers(); const pending = runE2e(f.config()); await vi.runAllTimersAsync(); const e = await pending;
    expect(e.success).toBe(false); expect(e.cleanup.sessionsDisposed).toBe(false); expect(n.items.every(item => item.kill.mock.calls.length === 1)).toBe(true);
  });
  it('polls asynchronous GET absence without issuing another DELETE', async () => {
    const f = fixture(), n = fakeNetwork(f); n.manager.getSandboxInfo.mockResolvedValueOnce({id: 'offline-1'});
    const e = await runE2e(f.config()); expect(e.success).toBe(true); expect(n.manager.getSandboxInfo).toHaveBeenCalledTimes(3);
    expect(n.items.every(item => item.kill.mock.calls.length === 1)).toBe(true);
  });
  it('does not accept generic failed lookups as deletion', async () => {
    const f = fixture(), n = fakeNetwork(f); n.manager.getSandboxInfo.mockRejectedValue(new Error('unreachable API'));
    const e = await runE2e(f.config()); expect(e.success).toBe(false); expect(e.cleanup.sandboxes.every(item => item.deleted === 'unconfirmed')).toBe(true);
  });
  it('retains acknowledged cancellation facts when status is unconfirmed', async () => {
    const f = fixture(), n = fakeNetwork(f); n.controls.unknownStatus = true;
    const e = await runE2e(f.config()); expect(e.success).toBe(false);
    expect(e.stages.find(stage => stage.name === 'cancellation_status')).toMatchObject({status: 'failed', facts: {commandId: 'offline-command', abortRequested: true, interruptAttempted: true, running: null}});
  });
});

describe('OFFLINE Ubuntu preflight and documentation contracts', () => {
  it('is valid read-only shell without install/deployment/mutation commands', () => {
    const path = new URL('../scripts/preflight-ubuntu.sh', import.meta.url), source = readFileSync(path, 'utf8');
    expect(spawnSync('bash', ['-n', path.pathname]).status).toBe(0);
    expect(source).not.toMatch(/^\s*(?:sudo|apt-get|apt|yum|mkdir|mount|umount|rm|docker\s+(?:run|build|pull|rm)|sysctl\s+-w)\b/m);
  });
  it('refuses existing work without modifying host paths', () => {
    const f = fixture(); const result = spawnSync('bash', [new URL('../scripts/preflight-ubuntu.sh', import.meta.url).pathname], {
      env: {...process.env, ...f.env, DSH_WORKSPACE_ROOT: f.root}, encoding: 'utf8', timeout: 15_000});
    expect(result.status).toBe(1); expect(result.stdout).toContain('WORK already exists'); expect(result.stdout).toContain('read-only');
    expect(readFileSync(new URL('../scripts/preflight-ubuntu.sh', import.meta.url), 'utf8')).not.toContain('mkdir -p');
  });
  it('reports failed read-only port/cluster queries as blockers, never as collision-free', () => {
    const f = fixture(), bin = join(f.root, 'bin'); mkdirSync(bin);
    for (const name of ['ss', 'kind']) writeFileSync(join(bin, name), '#!/bin/sh\nexit 42\n', {mode: 0o755});
    const result = spawnSync('bash', [new URL('../scripts/preflight-ubuntu.sh', import.meta.url).pathname], {
      env: {...process.env, ...f.env, DSH_WORKSPACE_ROOT: f.root, PATH: `${bin}:${process.env.PATH}`}, encoding: 'utf8', timeout: 15_000});
    expect(result.status).toBe(1); expect(result.stdout).toContain('cannot inspect TCP listeners');
    expect(result.stdout).toContain('cannot inspect kind clusters');
    expect(result.stdout).not.toContain('TCP port 18080 is unused');
  });
  it('executes the documented name generation with kind-compatible lowercase resource names', () => {
    const doc = readFileSync(new URL('../../../docs/examples/deepseek-harness-ubuntu.md', import.meta.url), 'utf8');
    const expressions = doc.match(/^export (?:RUN_ID|KIND_CLUSTER|RUSTFS_CONTAINER)=.*$/gm)!;
    expect(expressions).toHaveLength(3);
    const result = spawnSync('bash', ['-euo', 'pipefail', '-c', expressions.join('\n') + '\nprintf "%s\n" "$RUN_ID" "$KIND_CLUSTER" "$RUSTFS_CONTAINER"'], { encoding: 'utf8' });
    expect(result.status).toBe(0);
    const [run, cluster, container] = result.stdout.trim().split('\n');
    expect(run).toMatch(/^\d{8}t\d{6}z$/); expect(cluster).toBe(`dsh-ubuntu-e2e-${run}`);
    expect(cluster).toMatch(/^[a-z0-9.-]+$/); expect(container).toBe(`dsh-ubuntu-e2e-rustfs-${run}`);
  });
  it.each(['dsh-20261009T101335Z', 'dsh_invalid', 'dsh invalid', 'dsh-valid.20261009t101335z'])('checks kind name syntax before up: %s', name => {
    const f = fixture(), bin = join(f.root, 'name-bin'); mkdirSync(bin);
    // Never invoke a real cluster or Docker service while exercising the read-only preflight.
    for (const tool of ['kind', 'docker']) writeFileSync(join(bin, tool), '#!/bin/sh\nexit 0\n', { mode: 0o755 });
    const result = spawnSync('bash', [new URL('../scripts/preflight-ubuntu.sh', import.meta.url).pathname], {
      env: { ...process.env, ...f.env, DSH_WORKSPACE_ROOT: f.root, KIND_CLUSTER: name, PATH: `${bin}:${process.env.PATH}` }, encoding: 'utf8', timeout: 15_000 });
    if (/^[a-z0-9.-]+$/.test(name)) expect(result.stdout).toContain('OK: kind cluster name syntax');
    else { expect(result.status).toBe(1); expect(result.stdout).toContain('BLOCKER: KIND_CLUSTER must match ^[a-z0-9.-]+$'); }
  });
  it('aligns commands, variables, source pins and docs navigation', () => {
    const doc = readFileSync(new URL('../../../docs/examples/deepseek-harness-ubuntu.md', import.meta.url), 'utf8');
    for (const token of ['preflight-ubuntu.sh', 'integration-env.sh up', 'integration-env.sh status', 'integration-env.sh sdk-e2e',
      'integration-env.sh down', 'DSH_LIVE_E2E=1', 'DSH_EVIDENCE_PATH', 'DSH_DOWNLOADS_DIR', 'DSH_HOST_OBSERVATION_DIR', 'DSH_PNPM_EXECUTABLE', 'OPEN_SANDBOX_DOMAIN',
      'OPEN_SANDBOX_PROTOCOL', 'OPEN_SANDBOX_API_KEY', 'DSH_TEMPLATE_ID', '$WORK/template-id', 'pnpm run e2e', '/srv/app',
      'b702fbe71593df4f36b591e540445c251a698e7c', '4a7fcba5a4ded67e3856180936371673d390399b',
      'SKIP_TOOL_INSTALL=1', 'xfsprogs', 'action-time', '404', 'RepoDigests']) expect(doc, token).toContain(token);
    expect(doc).not.toMatch(/docker (?:system|volume|image) prune/);
    expect(doc).toContain('schemaVersion: 2'); expect(doc).toContain('all fourteen stage results');
    expect(readFileSync(new URL('../e2e.ts', import.meta.url), 'utf8')).not.toMatch(/contract-transport|dsh-llm-deepseek|DeepSeekAdapter|DSH_REAL_MODEL|transport\.interrupt\(/);
    for (const block of doc.matchAll(/```sh\n([\s\S]*?)```/g)) {
      expect(spawnSync('bash', ['-n'], {input: block[1], encoding: 'utf8'}).status).toBe(0);
    }
    const pin = readFileSync(new URL('../../../manifests/third-party/fast-sandbox.commit', import.meta.url), 'utf8');
    expect(doc).toContain(/^commit: (.*)$/m.exec(pin)![1]);
    const official = readFileSync(new URL('../../../scripts/fast-sandbox-env/integration-env.sh', import.meta.url), 'utf8');
    for (const action of ['up', 'status', 'sdk-e2e', 'down']) expect(official).toContain(`${action})`);
    expect(readFileSync(new URL('../../../docs/.vitepress/config.mts', import.meta.url), 'utf8')).toContain('/examples/deepseek-harness-ubuntu');
  });
});
