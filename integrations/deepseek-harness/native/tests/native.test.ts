// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { mkdir, readFile, symlink, writeFile } from 'node:fs/promises';
import { Context, RegistryService } from '@deepseek-ai/cordis';
import { assembleContextFor, type Agent } from '@deepseek-ai/dsh-agent';
import { FsError } from '@deepseek-ai/dsh-fs';
import { ToolCallId } from '@deepseek-ai/dsh-llm';
import type { SessionRequestId } from '@deepseek-ai/dsh-api-session-controller';
import { RemoteShell } from '../src/core/shell.js';
import { RemoteFileSystem } from '../src/core/filesystem.js';
import { SessionId } from '@deepseek-ai/dsh-session';
import * as ToolFs from '@deepseek-ai/dsh-tool-fs';
import { afterEach, describe, expect, it, vi } from 'vitest';
import native, { Config, createNativePlugin } from '../src/index.js';
import { remotePath, NATIVE_TOOL_NAMES } from '../src/tools.js';
import { NativeFileSystem } from '../src/services.js';
import { NativeOwner } from '../src/owner.js';
import type { NativeConfig } from '../src/config.js';
import type { NativeOwnerDependencies } from '../src/owner.js';
import { helperSdk } from './core-fixtures/helper-sdk.js';
import { deferred } from './core-fixtures/sdk-facade.js';
import { createControllerFixture, type ControllerStore } from './fixtures/controller.js';

const config: NativeConfig = { domain: 'offline.invalid', protocol: 'https', kind: 'create-template', templateId: 'offline-template', remoteCwd: '/workspace' };
const cleanup: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const close of cleanup.splice(0).reverse()) await close();
  vi.restoreAllMocks();
});
async function fixture(dependencies: NativeOwnerDependencies = {}, store?: ControllerStore) {
  const files = new Map<string, Awaited<ReturnType<typeof helperSdk>>>();
  const opened: string[] = [];
  const open: NonNullable<NativeOwnerDependencies['openBinding']> = async options => {
    opened.push(options.sessionId);
    let sdk = files.get(options.sessionId);
    if (!sdk) {
      sdk = await helperSdk(`sandbox-${options.sessionId}`, options.sessionId);
      files.set(options.sessionId, sdk); cleanup.push(sdk.cleanup);
    }
    return sdk.binding;
  };
  const app = await createControllerFixture(createNativePlugin({ openBinding: open, ...dependencies }), config, store ? { store } : {});
  cleanup.push(app.dispose);
  return { ...app, files, opened };
}
let serial = 0;
function tool(root: Context, agent: Agent | undefined, name: string, args: unknown, signal = new AbortController().signal) {
  return root.tools.execute({ callId: ToolCallId(`native-${++serial}`), name, arguments: args, ...(agent ? { agent } : {}), signal });
}
async function create(app: Awaited<ReturnType<typeof fixture>>, id: string) {
  await app.root.sessionController.create({ sessionId: SessionId(id), cwd: app.cwd, agentPreset: 'opensandbox' });
  const result = await app.root.sessionController.resolveAgent(SessionId(id));
  if ('error' in result) throw result.error;
  return result.agent;
}
const bash = (command = 'printf remote') => ({ description: 'Offline remote fixture command', command });

// SDK boundary is fake. The unchanged shell/filesystem providers and Python helper execute
// in disposable test directories, not a deployed sandbox or a real model account.
describe('actual native Web SessionController and public preset composition', () => {
  it('exports the native entry, local readiness, secret schema and seven lazy owned tools', async () => {
    expect(native.inject).not.toContain('agentLoop');
    expect(JSON.stringify(Config)).toContain('secret');
    const app = await fixture();
    expect(app.root.opensandboxNativeReady.preset).toBe('opensandbox');
    const agent = await create(app, 'lazy');
    expect(app.opened).toEqual([]);
    expect(app.root.tools.schemas(agent).map(row => row.name).sort()).toEqual([...NATIVE_TOOL_NAMES].sort());
    expect((await tool(app.root, agent, 'opensandbox_status', {})).isError).toBe(false);
    expect((await tool(app.root, agent, 'opensandbox_delete', { confirm_sandbox_id: 'unopened' })).isError).toBe(true);
    expect(app.opened).toEqual([]);
    await expect(app.root.fs.resolve('/etc/passwd')).rejects.toThrow(/disabled/);
    expect(app.root.fs.processPathFromHostPath(app.cwd)).toBeUndefined();
    await expect(app.root.sessionController.openWorkspacePath({ path: app.cwd }, new AbortController().signal)).rejects.toThrow();
    expect(app.hostCalls).toEqual([]);
  });

  it('runs shell-first and file-first through real provider/helper composition, with one concurrent acquisition', async () => {
    const app = await fixture();
    const first = await create(app, 'shell-first'), second = await create(app, 'file-first');
    const firstResult = await tool(app.root, first, 'bash', bash('printf alpha > note.txt'));
    expect(firstResult.isError).toBe(false);
    const concurrent = await Promise.all([
      tool(app.root, second, 'write', { file_path: 'note.txt', content: 'beta' }),
      tool(app.root, second, 'bash', bash()),
    ]);
    for (const result of concurrent) expect(result.isError, JSON.stringify(result)).toBe(false);
    expect(app.opened.sort()).toEqual(['file-first', 'shell-first']);
    expect((await tool(app.root, first, 'read', { file_path: 'note.txt' })).isError).toBe(false);
    expect((await tool(app.root, second, 'read', { file_path: 'note.txt' })).isError).toBe(false);
    expect(await readFile(app.files.get('shell-first')!.local('/workspace/note.txt'), 'utf8')).toBe('alpha');
    expect(await readFile(app.files.get('file-first')!.local('/workspace/note.txt'), 'utf8')).toBe('beta');
    expect(app.opened).toHaveLength(2);
  });

  it('preserves original Agent, execution identity, session header and symlink-sensitive paths', async () => {
    const app = await fixture(), agent = await create(app, 'paths');
    const originalOptions = agent.options, originalHeader = structuredClone(agent.session.header);
    const args = Object.freeze(bash());
    const seen: unknown[] = [];
    agent.ctx.on('tools/result', exec => { seen.push({ agent: exec.agent, arguments: exec.arguments }); });
    expect((await tool(app.root, agent, 'bash', args)).isError).toBe(false);
    const sdk = app.files.get('paths')!;
    await mkdir(sdk.local('/workspace/sub/child'), { recursive: true });
    await symlink('sub/child', sdk.local('/workspace/link'));
    await writeFile(sdk.local('/workspace/sub/note.txt'), 'symlink-parent');
    await writeFile(sdk.local('/workspace/note.txt'), 'lexical-parent');
    const result = await tool(app.root, agent, 'read', { file_path: 'link/../note.txt' });
    expect(result.isError).toBe(false);
    expect(JSON.stringify(result)).toContain('symlink-parent');
    expect(remotePath('link/../note.txt', '/workspace')).toBe('/workspace/link/../note.txt');
    expect(remotePath('/absolute', '/workspace')).toBe('/absolute');
    expect((await tool(app.root, agent, 'bash', { ...bash('pwd'), workdir: 'link/../' })).isError).toBe(false);
    expect(seen[0]).toEqual({ agent, arguments: args });
    expect(app.root.agents.get(agent.id)).toBe(agent);
    expect(agent.options).toBe(originalOptions);
    expect(agent.session.header).toEqual(originalHeader);
    agent.ctx.systemPrompt.section({ name: 'native-cwd-test', order: 999, text: 'Configured cwd: {{cwd}}' });
    const prompt = await app.root.systemPrompt.assemble(assembleContextFor(agent));
    expect(JSON.stringify(prompt)).toContain('/workspace');
  });

  it('keeps native FsError identity and per-session read-before-edit/version guards', async () => {
    const app = await fixture(), a = await create(app, 'observe-a'), b = await create(app, 'observe-b');
    await tool(app.root, a, 'bash', bash('printf before > note.txt'));
    await tool(app.root, b, 'bash', bash('printf before > note.txt'));
    const edit = { file_path: 'note.txt', old_string: 'before', new_string: 'after' };
    expect((await tool(app.root, a, 'edit', edit)).isError).toBe(true);
    await tool(app.root, a, 'read', { file_path: 'note.txt' });
    expect((await tool(app.root, b, 'edit', edit)).isError).toBe(true);
    expect((await tool(app.root, a, 'edit', edit)).isError).toBe(false);
    await tool(app.root, a, 'bash', bash('printf external > note.txt'));
    const stale = await tool(app.root, a, 'edit', { ...edit, old_string: 'external' });
    expect(stale.isError).toBe(true);
    expect(JSON.stringify(stale)).toMatch(/changed|stale|read.*again/i);
    const privateRoot = new Context();
    cleanup.push(() => privateRoot.fiber.dispose());
    const owner = new NativeOwner(privateRoot, config, candidate => candidate === a, { openBinding: async () => app.files.get('observe-a')!.binding });
    cleanup.push(() => owner.dispose());
    const route = owner.attach(a); owner.observeCreated(a, 'startup');
    const fs = new NativeFileSystem(privateRoot.isolate('fs'), { route });
    const target = await fs.resolve('absent.txt');
    await expect(fs.readText(target)).rejects.toBeInstanceOf(FsError);
  });

  it('denies sessionless, inherited, foreign same-scope and malformed calls without acquisition', async () => {
    const app = await fixture(), agent = await create(app, 'guard');
    const execute = vi.fn(async () => 'HOST');
    agent.ctx.tools.register({ name: 'foreign', description: 'Unapproved same-scope fixture', parameters: { type: 'object', properties: {} }, output: { schema: { type: 'string' }, render: (_a, value) => [{ type: 'text', text: String(value) }] }, execute });
    for (const result of [
      await tool(app.root, undefined, 'bash', bash()),
      await tool(app.root, agent, 'foreign', {}),
      await tool(app.root, agent, 'host_extra', {}),
      await tool(app.root, agent, 'bash', { ...bash(), workdir: ' ' }),
      await tool(app.root, agent, 'read', { file_path: '' }),
      await tool(app.root, agent, 'bash', { ...bash(), run_in_background: true }),
    ]) expect(result.isError).toBe(true);
    expect(execute).not.toHaveBeenCalled(); expect(app.opened).toEqual([]);
  });

  it('rolls back partial stock-tool composition and never calls a provider', async () => {
    const app = await fixture(), original = RegistryService.prototype.plugin;
    vi.spyOn(RegistryService.prototype, 'plugin').mockImplementation(function (this: RegistryService, plugin, configuration, stack) {
      if (plugin === ToolFs) throw new Error('offline partial-tool-setup failure');
      return original.call(this, plugin, configuration, stack);
    });
    await expect(create(app, 'partial')).rejects.toThrow();
    expect(app.root.agents.get(SessionId('partial'))).toBeUndefined();
    expect(app.root.sessions.get(SessionId('partial'))).toBeUndefined();
    expect(app.opened).toEqual([]);
  });


  it('uses the existing configured model for two rounds and resumes a new Agent on the same binding', async () => {
    const app = await fixture(), agent = await create(app, 'rounds');
    const options = agent.options;
    app.adapter.script('rounds', { tool: 'bash', arguments: bash('printf first > note.txt') }, { text: 'first round done' });
    await app.root.sessionController.prompt({ requestId: 'round-one' as SessionRequestId, sessionId: agent.id, mode: 'queue', content: [{ type: 'text', text: 'Write the first file.' }] }, new AbortController().signal);
    await agent.whenIdle();
    app.adapter.script('rounds', { tool: 'read', arguments: { file_path: 'note.txt' } }, { text: 'second round done' });
    await app.root.sessionController.prompt({ requestId: 'round-two' as SessionRequestId, sessionId: agent.id, mode: 'queue', content: [{ type: 'text', text: 'Read the existing file.' }] }, new AbortController().signal);
    await agent.whenIdle();
    expect(app.opened).toEqual(['rounds']);
    expect(app.modelRequests).toHaveLength(4);
    expect(app.modelRequests.every(request => request.provider === 'already-configured' && request.model === 'existing-model' && request.sessionId === 'rounds')).toBe(true);
    expect(agent.options).toBe(options);
    expect(app.root.agentDefaultModel.currentSelection()).toEqual(app.originalModelSelection);
    expect(agent.session.snapshotEvents().filter(event => event.type === 'tool/result').every(event => !event.data.message.isError)).toBe(true);
    const beforeResume = agent.session.snapshotEvents().length;
    await app.restartController();
    const result = await app.root.sessionController.resolveAgent(agent.id);
    if ('error' in result) throw result.error;
    expect(result.agent).not.toBe(agent);
    expect(result.agent.session.snapshotEvents().length).toBeGreaterThanOrEqual(beforeResume);
    expect((await tool(app.root, result.agent, 'read', { file_path: 'note.txt' })).isError).toBe(false);
    expect(app.opened).toEqual(['rounds']);
    expect(app.hostCalls).toEqual([]); await app.verifyBootstrap();
  });


  it('repeatedly drains old Agent generations while retaining only the session binding', async () => {
    const app = await fixture(); let agent = await create(app, 'many-generations');
    await tool(app.root, agent, 'write', { file_path: 'note.txt', content: 'retained' });
    const retired: Agent[] = [];
    for (let generation = 0; generation < 5; generation++) {
      retired.push(agent);
      await app.restartController();
      for (const old of retired) expect((await tool(app.root, old, 'read', { file_path: 'note.txt' })).isError).toBe(true);
      const resumed = await app.root.sessionController.resolveAgent(SessionId('many-generations'));
      if ('error' in resumed) throw resumed.error;
      agent = resumed.agent;
      expect(retired).not.toContain(agent);
      expect((await tool(app.root, agent, 'read', { file_path: 'note.txt' })).isError).toBe(false);
    }
    expect(app.root.agents.list()).toHaveLength(1);
    expect(app.opened).toEqual(['many-generations']);
    expect(app.lifecycle.filter(event => event === 'agent-disposed:many-generations')).toHaveLength(5);
    expect(app.hostCalls).toEqual([]);
  });

  it('cold resumes refuse implicit CREATE, and explicit reconnect is the only acquisition', async () => {
    const app = await fixture(), agent = await create(app, 'cold');
    await app.root.sessionController.prompt({ requestId: 'remember' as SessionRequestId, sessionId: agent.id, mode: 'queue', content: [{ type: 'text', text: 'Retain native history without opening a sandbox.' }] }, new AbortController().signal);
    await agent.whenIdle();
    const events = agent.session.snapshotEvents();
    await app.dispose();
    const calls: string[] = [];
    const sdk = await helperSdk('explicit-existing', 'cold'); cleanup.push(sdk.cleanup);
    const next = await fixture({ openBinding: async options => {
      calls.push(options.kind);
      expect(options).toMatchObject({ kind: 'connect', sandboxId: 'explicit-existing' });
      return sdk.binding;
    } }, app.store);
    const resumed = await next.root.sessionController.resolveAgent(SessionId('cold'));
    if ('error' in resumed) throw resumed.error;
    expect(resumed.agent.session.snapshotEvents().slice(0, events.length)).toEqual(events);
    expect((await tool(next.root, resumed.agent, 'read', { file_path: 'note.txt' })).isError).toBe(true);
    expect(calls).toEqual([]);
    expect((await tool(next.root, resumed.agent, 'opensandbox_status', {})).value).toMatchObject({ state: 'reconnect-required' });
    expect((await tool(next.root, resumed.agent, 'opensandbox_reconnect', { sandbox_id: 'explicit-existing' })).isError).toBe(false);
    expect(calls).toEqual(['connect']);
    expect((await tool(next.root, resumed.agent, 'write', { file_path: 'note.txt', content: 'reconnected' })).isError).toBe(false);
    expect(calls).toEqual(['connect']);
  });

  it('keeps tool admission closed throughout asynchronous native composition', async () => {
    const app = await fixture(), started = deferred<void>(), gate = deferred<void>(), original = RegistryService.prototype.plugin;
    vi.spyOn(RegistryService.prototype, 'plugin').mockImplementation(function (this: RegistryService, plugin, configuration, stack) {
      if (plugin === ToolFs) return original.call(this, { ...ToolFs, async apply(ctx: Context, configured: ToolFs.Config) { started.resolve(); await gate.promise; ToolFs.apply(ctx, configured); } }, configuration, stack);
      return original.call(this, plugin, configuration, stack);
    });
    const creating = create(app, 'pending');
    await started.promise;
    const unpublishedToCaller = app.root.agents.get(SessionId('pending'))!;
    expect(unpublishedToCaller).toBeDefined();
    expect((await tool(app.root, unpublishedToCaller, 'bash', bash())).isError).toBe(true);
    expect(app.opened).toEqual([]); expect(app.modelRequests).toEqual([]);
    gate.resolve(); const agent = await creating;
    expect((await tool(app.root, agent, 'bash', bash())).isError).toBe(false);
  });

  it('latches unknown creation errors without leaking SDK diagnostics or falling back', async () => {
    const open = vi.fn(async () => { throw new Error('private-endpoint.invalid secret-api-key'); });
    const app = await fixture({ openBinding: open }), agent = await create(app, 'unknown');
    const results = await Promise.all([tool(app.root, agent, 'bash', bash()), tool(app.root, agent, 'read', { file_path: 'note.txt' })]);
    results.push(await tool(app.root, agent, 'bash', bash()));
    expect(results.every(result => result.isError)).toBe(true);
    expect(JSON.stringify(results)).not.toMatch(/private-endpoint|secret-api-key/);
    expect(open).toHaveBeenCalledOnce(); expect(app.hostCalls).toEqual([]);
    expect((await tool(app.root, agent, 'opensandbox_status', {})).value).toMatchObject({ state: 'outcome-unknown', remoteExistence: 'unverified' });
  });

  it('explicit delete does not recreate an unavailable binding or delete another ID', async () => {
    const app = await fixture(), agent = await create(app, 'delete');
    await tool(app.root, agent, 'bash', bash());
    expect((await tool(app.root, agent, 'opensandbox_delete', { confirm_sandbox_id: 'different' })).isError).toBe(true);
    expect((await tool(app.root, agent, 'opensandbox_delete', { confirm_sandbox_id: 'sandbox-delete' })).isError).toBe(false);
    expect((await tool(app.root, agent, 'opensandbox_status', {})).value).toMatchObject({ state: 'killed', remoteExistence: 'deleted' });
    expect((await tool(app.root, agent, 'bash', bash())).isError).toBe(true);
    expect(app.opened).toEqual(['delete']);
  });


  it('keeps local status and explicit delete available after provider drain failure', async () => {
    const app = await fixture({ createProviders: async (_agent, binding, options) => {
      const ctx = new Context();
      await ctx.plugin(RemoteShell, { binding, settlementTimeoutMs: options.settlementTimeoutMs });
      await ctx.plugin(RemoteFileSystem, { binding, settlementTimeoutMs: options.settlementTimeoutMs });
      const shell = ctx.shell as RemoteShell, fs = ctx.fs as RemoteFileSystem;
      await fs.ready();
      return { binding, shell, fs, async dispose() { await ctx.fiber.dispose(); throw new Error('offline drain failure'); } };
    } });
    const agent = await create(app, 'drain');
    expect((await tool(app.root, agent, 'bash', bash())).isError).toBe(false);
    expect((await tool(app.root, agent, 'opensandbox_reconnect', { sandbox_id: 'sandbox-drain' })).isError).toBe(true);
    expect((await tool(app.root, agent, 'opensandbox_status', {})).value).toMatchObject({ state: 'drain-failed' });
    expect((await tool(app.root, agent, 'bash', bash())).isError).toBe(true);
    expect((await tool(app.root, agent, 'opensandbox_delete', { confirm_sandbox_id: 'sandbox-drain' })).isError).toBe(false);
    expect((await tool(app.root, agent, 'opensandbox_status', {})).value).toMatchObject({ state: 'killed' });
    expect(app.opened).toEqual(['drain']);
  });

  it('reports local status while explicit deletion waits for shared opening', async () => {
    const started = deferred<void>(), release = deferred<void>();
    const sdk = await helperSdk('sandbox-delete-pending', 'delete-pending'); cleanup.push(sdk.cleanup);
    const app = await fixture({ openBinding: async () => { started.resolve(); await release.promise; return sdk.binding; } });
    const agent = await create(app, 'delete-pending');
    const running = tool(app.root, agent, 'bash', bash()); await started.promise;
    const deletion = tool(app.root, agent, 'opensandbox_delete', { confirm_sandbox_id: 'sandbox-delete-pending' });
    await new Promise(resolve => setImmediate(resolve));
    expect((await tool(app.root, agent, 'opensandbox_status', {})).value).toMatchObject({ state: 'opening', operation: 'delete' });
    release.resolve();
    expect((await deletion).isError).toBe(false); expect((await running).isError).toBe(true);
    expect(sdk.calls).toEqual([]);
  });


  it('awaits actual provider command draining during native controller-owned Agent disposal', async () => {
    const app = await fixture(), agent = await create(app, 'draining-command');
    await tool(app.root, agent, 'bash', bash());
    const sdk = app.files.get('draining-command')!, started = deferred<void>();
    sdk.controls.afterLaunch = argv => { if (argv[1] === '-c' && argv[3] === '') started.resolve(); };
    const running = tool(app.root, agent, 'bash', bash('exec sleep 30'));
    await started.promise;
    await app.restartController();
    expect((await running).isError).toBe(true);
    expect(sdk.commands.interrupt).toHaveBeenCalled();
    expect(await sdk.artifactRoots()).toEqual([]);
    expect((await tool(app.root, agent, 'read', { file_path: 'note.txt' })).isError).toBe(true);
    expect(sdk.binding.state).toBe('open');
    expect(app.opened).toEqual(['draining-command']);
  });

  it('closes late opening on unload without launching a remote operation', async () => {
    const started = deferred<void>(), release = deferred<void>();
    const sdk = await helperSdk('sandbox-late', 'late'); cleanup.push(sdk.cleanup);
    const app = await fixture({ openBinding: async () => { started.resolve(); await release.promise; return sdk.binding; } });
    const agent = await create(app, 'late');
    const running = tool(app.root, agent, 'bash', bash()); await started.promise;
    const unload = app.nativeFiber.dispose();
    await new Promise(resolve => setImmediate(resolve)); release.resolve();
    await unload;
    expect((await running).isError).toBe(true);
    expect(sdk.binding.state).toBe('closed');
    expect(sdk.calls).toEqual([]); expect(app.hostCalls).toEqual([]);
  });

  it('cancels one caller without cancelling shared opening and denies all operations after unload', async () => {
    const ready = deferred<void>(), started = deferred<void>();
    let opens = 0;
    const sdk = await helperSdk('sandbox-shared', 'shared'); cleanup.push(sdk.cleanup);
    const app = await fixture({ openBinding: async () => { opens++; started.resolve(); await ready.promise; return sdk.binding; } });
    const agent = await create(app, 'shared'), abort = new AbortController();
    const a = tool(app.root, agent, 'bash', bash(), abort.signal);
    const b = tool(app.root, agent, 'write', { file_path: 'note.txt', content: 'survivor' });
    await started.promise; abort.abort(); ready.resolve();
    expect((await a).isError).toBe(true); expect((await b).isError).toBe(false); expect(opens).toBe(1);
    await app.nativeFiber.dispose();
    expect((await tool(app.root, agent, 'bash', bash())).isError).toBe(true);
    expect((await tool(app.root, agent, 'opensandbox_status', {})).isError).toBe(true);
    expect(opens).toBe(1);
  });
});
