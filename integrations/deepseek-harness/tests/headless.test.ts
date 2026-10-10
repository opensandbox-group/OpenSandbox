// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { readFile, writeFile } from 'node:fs/promises';
import { inspect } from 'node:util';
import { Context, RegistryService } from '@deepseek-ai/cordis';
import AgentRegistry, { type Agent } from '@deepseek-ai/dsh-agent';
import LlmRuntime, { LlmAdapter, LlmError, ToolCallId, resolveRetryPolicy } from '@deepseek-ai/dsh-llm';
import type { GenerateOptions, LlmResolvedModelInfo, StreamChunk } from '@deepseek-ai/dsh-llm';
import * as ToolBash from '@deepseek-ai/dsh-tool-bash';
import * as ToolFs from '@deepseek-ai/dsh-tool-fs';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { BoundSandbox } from '../src/index.js';
import { helperSdk } from './fixtures/helper-sdk.js';
import { deferred } from './fixtures/sdk-facade.js';

import * as api from '../src/index.js';

const cleanups: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
  vi.restoreAllMocks();
});
function textResponse(text = 'done'): StreamChunk[] {
  return [{ type: 'block-start', index: 0, blockType: 'text' }, { type: 'text-delta', index: 0, text },
    { type: 'block-end', index: 0, block: { type: 'text', text } }, { type: 'finish', reason: { kind: 'stop' } }];
}
function toolResponse(id: string, name: string, args: object): StreamChunk[] {
  const callId = ToolCallId(id), argumentsJson = JSON.stringify(args);
  return [{ type: 'block-start', index: 0, blockType: 'tool-call' },
    { type: 'tool-call-delta', index: 0, id: callId, name, argumentsDelta: argumentsJson },
    { type: 'block-end', index: 0, block: { type: 'tool-call', id: callId, name, arguments: argumentsJson } },
    { type: 'finish', reason: { kind: 'tool-calls' } }];
}
class ScriptedAdapter extends LlmAdapter {
  readonly requests: GenerateOptions[] = [];
  constructor(readonly script: (StreamChunk[] | ((request: GenerateOptions) => StreamChunk[]))[], readonly credential = 'private-adapter-key') { super(); }
  override async resolveModel(provider: string, model: string, _signal?: AbortSignal): Promise<LlmResolvedModelInfo> {
    return { provider, id: model, name: model };
  }
  async *stream(request: GenerateOptions): AsyncIterable<StreamChunk> {
    this.requests.push(request);
    const step = this.script.shift(); if (!step) throw new Error('Script exhausted.');
    for (const chunk of typeof step === 'function' ? step(request) : step) yield chunk;
  }
}
async function fixture(id = 'sandbox-a', sessionId = 'session-a') {
  const f = await helperSdk(id, sessionId); cleanups.push(f.cleanup); return f;
}
async function create(binding: BoundSandbox, adapter: LlmAdapter, provider = 'scripted', model = 'offline-model') {
  expect(api.createHeadlessSession, 'managed headless composition must be exported').toBeTypeOf('function');
  const session = await api.createHeadlessSession({ binding, adapter, provider, model });
  cleanups.push(() => session.dispose()); return session;
}
function results(agent: Agent) {
  return agent.session.snapshotEvents().filter(event => event.type === 'tool/result').map(event => event.data.message);
}
function output(request: GenerateOptions) {
  return request.messages.filter(message => message.role === 'tool').flatMap(message => message.content)
    .flatMap(block => block.type === 'text' ? [block.text] : []).join('\n');
}
function loop(content: string) {
  return [toolResponse('bash', 'bash', { description: 'Create remote file', command: `printf '%s' '${content}' > note.txt` }),
    toolResponse('read', 'read', { file_path: 'note.txt' }),
    toolResponse('edit', 'edit', { file_path: 'note.txt', old_string: content, new_string: `${content}-edited` }),
    textResponse()];
}

describe('private headless composition over published dsh and actual Python helper', () => {
  it('runs the actual Agent bash/read/edit registry loop with an observed version guard', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter(loop('alpha'));
    const session = await create(f.binding, adapter);
    const dispatched: string[] = [];
    session.agent.ctx.on('tools/result', exec => { dispatched.push(exec.name); });
    await session.run('Perform the scripted remote file loop.');
    expect(dispatched).toEqual(['bash', 'read', 'edit']);
    expect(await readFile(f.local('/workspace/note.txt'), 'utf8')).toBe('alpha-edited');
    expect(adapter.requests).toHaveLength(4);
    expect(output(adapter.requests[2]!)).toContain('alpha');
    expect(results(session.agent).every(result => !result.isError)).toBe(true);
    const edits = f.uploads.filter(item => item.path.endsWith('/request.json')).map(item => JSON.parse(Buffer.from(item.data).toString()))
      .filter(request => request.operation === 'edit-text');
    expect(edits).toHaveLength(1);
    expect(edits[0].args.expected.version).toEqual(expect.any(String));
    expect(edits[0].args.expected.version.length).toBeGreaterThan(0);
    expect(session.descriptor).toEqual(f.binding.descriptor);
    expect(Object.isFrozen(session.descriptor)).toBe(true);
    expect(session.agent.id).toBe('session-a'); expect(session.agent.session.header.cwd).toBe('/workspace');
  });
  it('rejects unobserved edits in the actual Agent loop', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/note.txt'), 'alpha');
    const adapter = new ScriptedAdapter([toolResponse('edit', 'edit', { file_path: 'note.txt', old_string: 'alpha', new_string: 'changed' }), textResponse()]);
    const session = await create(f.binding, adapter); await session.run('Try editing before reading.');
    expect(results(session.agent)[0]).toMatchObject({ isError: true });
    expect(output(adapter.requests[1]!)).toMatch(/file has not been read/i);
    expect(await readFile(f.local('/workspace/note.txt'), 'utf8')).toBe('alpha');
    expect(f.uploads.filter(item => item.path.endsWith('/request.json')).some(item => JSON.parse(Buffer.from(item.data).toString()).operation === 'edit-text')).toBe(false);
  });
  it('rejects a stale observed guard after a shell mutation', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/note.txt'), 'alpha');
    const adapter = new ScriptedAdapter([toolResponse('read', 'read', { file_path: 'note.txt' }),
      toolResponse('mutate', 'bash', { description: 'Change observed file', command: "printf beta > note.txt" }),
      toolResponse('edit', 'edit', { file_path: 'note.txt', old_string: 'beta', new_string: 'changed' }), textResponse()]);
    const session = await create(f.binding, adapter); await session.run('Check stale read.');
    expect(results(session.agent).at(-1)).toMatchObject({ isError: true });
    expect(output(adapter.requests[3]!)).toMatch(/changed|stale|read.*again/i);
    expect(await readFile(f.local('/workspace/note.txt'), 'utf8')).toBe('beta');
  });
  it('isolates identical cwd/filename across Contexts, adapters, tools and target capabilities', async () => {
    const a = await fixture('sandbox-a', 'session-a'), b = await fixture('sandbox-b', 'session-b');
    a.boundary.connectionConfig.apiKey = 'sdk-credential-a'; b.boundary.connectionConfig.apiKey = 'sdk-credential-b';
    const aa = new ScriptedAdapter(loop('alpha'), 'credential-a'), ba = new ScriptedAdapter(loop('beta'), 'credential-b');
    const sa = await create(a.binding, aa, 'provider-a', 'model-a'), sb = await create(b.binding, ba, 'provider-b', 'model-b');
    expect(sa.agent.ctx.root).not.toBe(sb.agent.ctx.root);
    expect(sa.agent.ctx.tools.get('bash')).not.toBe(sb.agent.ctx.tools.get('bash'));
    expect(sa.agent.ctx.tools.get('read')).not.toBe(sb.agent.ctx.tools.get('read'));
    await Promise.all([sa.run('Run alpha.'), sb.run('Run beta.')]);
    expect(await readFile(a.local('/workspace/note.txt'), 'utf8')).toBe('alpha-edited');
    expect(await readFile(b.local('/workspace/note.txt'), 'utf8')).toBe('beta-edited');
    expect(aa.requests.every(request => request.sessionId === 'session-a' && request.provider === 'provider-a' && request.model === 'model-a')).toBe(true);
    expect(ba.requests.every(request => request.sessionId === 'session-b' && request.provider === 'provider-b' && request.model === 'model-b')).toBe(true);
    expect(JSON.stringify(aa.requests)).not.toContain('credential-b'); expect(JSON.stringify(ba.requests)).not.toContain('credential-a');
    for (const session of [sa, sb]) expect(JSON.stringify(session.descriptor)).not.toMatch(/credential|apiKey|Authorization/);
    for (const adapter of [aa, ba]) expect(JSON.stringify(adapter.requests)).not.toMatch(/sdk-credential-[ab]/);
    expect(a.boundary.connectionConfig.apiKey).toBe('sdk-credential-a'); expect(b.boundary.connectionConfig.apiKey).toBe('sdk-credential-b');
    const ta = await sa.agent.ctx.root.fs.resolve('note.txt'), tb = await sb.agent.ctx.root.fs.resolve('note.txt');
    expect(ta.targetKey).not.toBe(tb.targetKey);
    await expect(sa.agent.ctx.root.fs.readText(tb)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    await expect(sb.agent.ctx.root.fs.readText(ta)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(a.calls.find(call => call.argv[1] === '-c' && call.argv[3] === '')?.options).toMatchObject({ envs: { DSH_SESSION_ID: 'session-a' } });
    expect(b.calls.find(call => call.argv[1] === '-c' && call.argv[3] === '')?.options).toMatchObject({ envs: { DSH_SESSION_ID: 'session-b' } });
  });
  it('awaits verified helper readiness before Agent creation', async () => {
    const f = await fixture(), gate = deferred<void>(), started = deferred<void>();
    f.controls.beforeLaunch = async argv => { if (argv[1]?.endsWith('/fs_helper.py')) { started.resolve(); await gate.promise; } };
    const createSpy = vi.spyOn(AgentRegistry.prototype, 'create');
    expect(api.createHeadlessSession).toBeTypeOf('function');
    const pending = create(f.binding, new ScriptedAdapter([textResponse()]));
    try { await Promise.race([started.promise, pending.then(() => undefined)]); expect(createSpy).not.toHaveBeenCalled(); } finally { gate.resolve(); }
    const session = await pending; expect(createSpy).toHaveBeenCalledOnce();
    expect(f.requestCount()).toBe(2); expect(session.agent.status).toBe('idle');
  });
  it('uses only the approved plugin allowlist and foreground tool schema', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([textResponse()]);
    const mountSpy = vi.spyOn(RegistryService.prototype, 'plugin');
    const session = await create(f.binding, adapter); await session.run('Show available tools.');
    expect(mountSpy.mock.calls.filter((_, index) => (mountSpy.mock.contexts[index] as RegistryService).ctx.fiber === session.agent.ctx.root.fiber).map(([plugin]) => (plugin as { name?: string }).name)).toEqual([
      'LlmRuntime', 'SessionStore', 'SessionProjectionRegistry', 'SystemPrompt', 'ToolRuntime', 'AgentRegistry', 'AgentLoop',
      'RemoteShell', 'RemoteFileSystem', 'shell-env', 'tool-bash', 'tool-fs', 'fs-observation-policy',
    ]);
    expect(mountSpy.mock.calls.find(([plugin]) => plugin === ToolBash)?.[1]).toEqual({ enableRunInBackground: false, promoteOnTimeout: false });
    for (const service of ['jobs', 'subprocess', 'fsSearch', 'ptc', 'attachments', 'sandboxPolicy']) expect(session.agent.ctx.get(service)).toBeUndefined();
    const tools = adapter.requests[0]!.tools ?? adapter.requests[0]!.toolHistory?.tools;
    expect(tools?.map(tool => tool.name).sort()).toEqual(['bash', 'edit', 'read', 'write']);
    const bash = session.agent.ctx.tools.get('bash')!;
    expect(JSON.stringify(bash.parameters)).not.toContain('run_in_background');
    expect(JSON.stringify(bash.parameters)).not.toContain('sandbox_permissions');
  });
  it.each([
    { run_in_background: true },
    { sandbox_permissions: 'read-only', justification: 'Require a narrower policy' },
  ])('rejects hand-crafted unsupported bash arguments %j without launch', async args => {
    const f = await fixture(), adapter = new ScriptedAdapter([toolResponse('bad-bash', 'bash', { description: 'Must fail', command: 'touch unsafe', ...args }), textResponse()]);
    const session = await create(f.binding, adapter), count = f.calls.length;
    await session.run('Exercise the unsupported argument.');
    expect(results(session.agent)[0]).toMatchObject({ isError: true });
    expect(f.calls).toHaveLength(count); await expect(readFile(f.local('/workspace/unsafe'))).rejects.toMatchObject({ code: 'ENOENT' });
  });
  it('awaits an asynchronous kernel plugin before mounting the next plugin', async () => {
    const f = await fixture(), gate = deferred<void>(), started = deferred<void>(), original = RegistryService.prototype.plugin;
    const createSpy = vi.spyOn(AgentRegistry.prototype, 'create');
    let nextMountStarted = false;
    vi.spyOn(RegistryService.prototype, 'plugin').mockImplementation(function (this: RegistryService, plugin, config, stack) {
      if (plugin === LlmRuntime) return original.call(this, async (ctx: Context) => {
        new LlmRuntime(ctx); started.resolve(); await gate.promise;
      }, undefined, stack);
      nextMountStarted = true; return original.call(this, plugin, config, stack);
    });
    const pending = create(f.binding, new ScriptedAdapter([textResponse()]));
    try {
      await Promise.race([started.promise, pending.then(() => undefined)]);
      expect(nextMountStarted).toBe(false); expect(createSpy).not.toHaveBeenCalled();
    } finally { gate.resolve(); }
    await pending; expect(createSpy).toHaveBeenCalledOnce();
  });
  it('returns foreground timeout evidence without promoting or leaving a job', async () => {
    const f = await fixture(), session = await create(f.binding, new ScriptedAdapter([]));
    const result = await session.agent.ctx.tools.execute({ agent: session.agent, callId: ToolCallId('timeout'), name: 'bash',
      arguments: { description: 'Short deadline', command: 'exec sleep 2', timeoutMs: 30 }, signal: new AbortController().signal });
    expect(result).toMatchObject({ isError: false, value: { kind: 'foreground', timedOut: true } });
    expect(f.commands.interrupt).toHaveBeenCalled(); expect(session.agent.ctx.get('jobs')).toBeUndefined();
  });
  it('cleans helper readiness failure without publishing an Agent or changing binding lifecycle', async () => {
    const f = await fixture(), createSpy = vi.spyOn(AgentRegistry.prototype, 'create');
    f.controls.transformResponse = bytes => Buffer.from('{invalid-json');
    await expect(create(f.binding, new ScriptedAdapter([]))).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(createSpy).not.toHaveBeenCalled(); expect(f.binding.state).toBe('open');
    expect(f.sandbox.kill).not.toHaveBeenCalled(); expect(f.sandbox.close).not.toHaveBeenCalled();
  });
  it('fails closed if a required mounted service is absent, and disposes partial Context', async () => {
    const f = await fixture(), original = RegistryService.prototype.plugin; let root: Context | undefined;
    vi.spyOn(RegistryService.prototype, 'plugin').mockImplementation(function (this: RegistryService, plugin, config, stack) {
      root = this.ctx.root; return original.call(this, plugin === LlmRuntime ? () => undefined : plugin, config, stack);
    });
    await expect(create(f.binding, new ScriptedAdapter([]))).rejects.toThrow(/required.*llm|llm.*missing/i);
    expect(root!.registry.size).toBe(0); expect(f.binding.state).toBe('open');
  });
  it('cleans providers and root Context if a later tool mount fails', async () => {
    const f = await fixture(), original = RegistryService.prototype.plugin; let root: Context | undefined;
    vi.spyOn(RegistryService.prototype, 'plugin').mockImplementation(function (this: RegistryService, plugin, config, stack) {
      root = this.ctx.root; if (plugin === ToolFs) throw new Error('tool mount failed'); return original.call(this, plugin, config, stack);
    });
    await expect(create(f.binding, new ScriptedAdapter([]))).rejects.toThrow('tool mount failed');
    expect(root!.registry.size).toBe(0); expect(await f.artifactRoots()).toEqual([]);
    expect(f.binding.state).toBe('open'); expect(f.sandbox.kill).not.toHaveBeenCalled(); expect(f.sandbox.close).not.toHaveBeenCalled();
  });
  it('drains active Agent tools and provider commands before disposing root, without closing the binding', async () => {
    const f = await fixture(), started = deferred<void>();
    f.controls.afterLaunch = argv => { if (argv[1] === '-c' && argv[3] === '') started.resolve(); };
    const session = await create(f.binding, new ScriptedAdapter([toolResponse('long', 'bash', { description: 'Wait', command: 'exec sleep 30' }), textResponse()]));
    const root = session.agent.ctx.root, phases: string[] = [], original = root.fiber.dispose;
    vi.spyOn(root.fiber, 'dispose').mockImplementation(async () => {
      phases.push('root'); expect(f.commands.interrupt).toHaveBeenCalled(); expect(session.agent.status).toBe('idle');
      await expect(root.fs.resolve('note.txt')).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
      await original();
    });
    const running = session.run('Run the long command.'); await started.promise;
    const disposed = session.dispose(); await Promise.all([running, disposed]); await session.dispose();
    expect(phases).toEqual(['root']); expect(root.registry.size).toBe(0);
    expect(f.sandbox.kill).not.toHaveBeenCalled(); expect(f.sandbox.close).not.toHaveBeenCalled(); expect(f.binding.state).toBe('open');
    await expect(session.run('Too late.')).rejects.toThrow(/disposed/i);
  });
  it('rejects a terminal binding before composing anything', async () => {
    const f = await fixture(); await f.binding.close();
    await expect(create(f.binding, new ScriptedAdapter([]))).rejects.toMatchObject({ code: 'binding_closed' });
    expect(f.calls).toEqual([]);
  });
});


const diagnosticCredential = 'fake-model-credential-boundary-sentinel';
function adapterDiagnostic(code = diagnosticCredential) {
  return Object.assign(new Error(`Authorization: Bearer ${diagnosticCredential}`, { cause: new Error(diagnosticCredential) }), {
    code, headers: { Authorization: diagnosticCredential }, request: { body: diagnosticCredential },
  });
}
function expectSafeAdapterError(error: unknown) {
  expect(error).toBeInstanceOf(LlmError);
  expect(inspect(error, { depth: 20 })).not.toContain(diagnosticCredential);
  expect(JSON.stringify(error)).not.toContain(diagnosticCredential);
  expect(error).not.toHaveProperty('cause'); expect(error).not.toHaveProperty('headers'); expect(error).not.toHaveProperty('request');
  expect((error as Error).message).toMatch(/LLM adapter .* failed/);
}

describe('headless adapter failure boundary', () => {
  it.each(['providerInfo', 'providerRetryPolicy'] as const)('sanitizes synchronous %s registration failures before exposing composition errors', async method => {
    const f = await fixture(), source = adapterDiagnostic(), adapter = new ScriptedAdapter([]);
    vi.spyOn(adapter, method).mockImplementation(() => { throw source; });
    const error = await create(f.binding, adapter).then(() => undefined, error => error);
    expect(error).not.toBe(source); expectSafeAdapterError(error); expect(f.calls).toEqual([]);
  });
  it('does not retain the original adapter error inside partial-init cleanup aggregates', async () => {
    const f = await fixture(), source = adapterDiagnostic(), adapter = new ScriptedAdapter([]), original = RegistryService.prototype.plugin;
    vi.spyOn(adapter, 'providerInfo').mockImplementation(() => { throw source; });
    vi.spyOn(RegistryService.prototype, 'plugin').mockImplementation(function (this: RegistryService, plugin, config, stack) {
      const root = this.ctx.root;
      if (!vi.isMockFunction(root.fiber.dispose)) {
        const dispose = root.fiber.dispose;
        vi.spyOn(root.fiber, 'dispose').mockImplementation(async () => { await dispose(); throw new Error('Root cleanup failed.'); });
      }
      return original.call(this, plugin, config, stack);
    });
    const error = await create(f.binding, adapter).then(() => undefined, error => error);
    expect(error).toBeInstanceOf(AggregateError); expect(inspect(error, { depth: 20 })).not.toContain(diagnosticCredential);
    expect(error.errors).not.toContain(source); expectSafeAdapterError(error.errors[0]);
  });
  it.each(['listModels', 'resolveModel', 'imageRequestPricing', 'priceImages'] as const)('sanitizes public %s callback failures', async method => {
    const f = await fixture(), source = adapterDiagnostic(), adapter = new ScriptedAdapter([]);
    if (method === 'listModels') vi.spyOn(adapter, 'listModels').mockRejectedValue(source);
    if (method === 'resolveModel') vi.spyOn(adapter, 'resolveModel').mockRejectedValue(source);
    if (method === 'imageRequestPricing') vi.spyOn(adapter, 'imageRequestPricing').mockImplementation(() => { throw source; });
    if (method === 'priceImages') vi.spyOn(adapter, 'imageRequestPricing').mockReturnValue({ priceImages() { throw source; } });
    const session = await create(f.binding, adapter), llm = session.agent.ctx.root.llm;
    const error = await Promise.resolve().then<unknown>(() => {
      if (method === 'listModels') return llm.listModels('scripted');
      if (method === 'resolveModel') return llm.resolveModelInfo('scripted', 'offline-model');
      const pricing = llm.imageRequestPricing('scripted', 'offline-model');
      if (method === 'priceImages') return pricing!.priceImages([]);
      return pricing;
    }).then(() => undefined, error => error);
    expect(error).not.toBe(source); expectSafeAdapterError(error);
  });
  it.each(['resolveModel', 'prepareCall', 'stream-call', 'stream-next', 'prepared-stream-call', 'prepared-stream-next', 'failure-chunk', 'aborted-chunk'] as const)(
    'keeps %s adapter diagnostics out of Agent error events and durable failure records', async method => {
      const f = await fixture(), source = adapterDiagnostic(), adapter = new ScriptedAdapter([]);
      if (method === 'resolveModel') vi.spyOn(adapter, 'resolveModel').mockRejectedValue(source);
      if (method === 'prepareCall') vi.spyOn(adapter, 'prepareCall').mockRejectedValue(source);
      if (method === 'stream-call') vi.spyOn(adapter, 'stream').mockImplementation(() => { throw source; });
      if (method === 'stream-next') vi.spyOn(adapter, 'stream').mockImplementation(async function* () { throw source; });
      if (method.startsWith('prepared-stream')) vi.spyOn(adapter, 'prepareCall').mockResolvedValue({
        model: { provider: 'scripted', id: 'offline-model', name: 'offline-model' },
        stream: method === 'prepared-stream-call' ? () => { throw source; } : async function* () { throw source; },
      });
      if (method === 'failure-chunk' || method === 'aborted-chunk') vi.spyOn(adapter, 'stream').mockImplementation(async function* () {
        yield { type: 'finish', reason: { kind: method === 'failure-chunk' ? 'error' : 'aborted', failure: { message: diagnosticCredential, code: diagnosticCredential } } };
      });
      const session = await create(f.binding, adapter), errors: unknown[] = [];
      session.agent.ctx.on('agent/error', payload => { errors.push(payload.error); });
      await session.run('Exercise the offline adapter failure.');
      expect(errors).toHaveLength(1); expect(errors).not.toContain(source); expectSafeAdapterError(errors[0]);
      const events = session.agent.session.snapshotEvents();
      expect(JSON.stringify(events)).not.toContain(diagnosticCredential);
      expect(events.filter(event => event.type === 'turn/end').at(-1)).toBeDefined();
    });
  it('sanitizes async iterator cleanup failures without swallowing cleanup or changing the receiver', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([]), source = adapterDiagnostic();
    const iterator = { next: vi.fn().mockResolvedValue({ done: false, value: { type: 'block-start', index: 0, blockType: 'text' } }),
      return: vi.fn().mockImplementation(function (this: unknown) { expect(this).toBe(iterator); return Promise.reject(source); }),
      [Symbol.asyncIterator]() { return this; } };
    vi.spyOn(adapter, 'stream').mockReturnValue(iterator);
    const session = await create(f.binding, adapter), stream = session.agent.ctx.root.llm.stream({ provider: 'scripted', model: 'offline-model', messages: [] })[Symbol.asyncIterator]();
    await stream.next(); const error = await stream.return!().catch(error => error);
    expectSafeAdapterError(error); expect(iterator.return).toHaveBeenCalledOnce();
  });
  it('preserves known retry classification and safe numeric evidence while dropping all diagnostic strings and causes', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([]), source = new LlmError(diagnosticCredential, 'RATE_LIMIT', {
      status: 429, providerRetryAfterMs: 1200, requestId: diagnosticCredential as never, cause: adapterDiagnostic(),
    });
    vi.spyOn(adapter, 'resolveModel').mockRejectedValue(source);
    const session = await create(f.binding, adapter);
    const error = await session.agent.ctx.root.llm.resolveModelInfo('scripted', 'offline-model').catch(error => error);
    expectSafeAdapterError(error); expect(error).toMatchObject({ code: 'RATE_LIMIT', failure: { status: 429, providerRetryAfterMs: 1200 } });
    expect(error.failure).not.toHaveProperty('requestId');
    expect(session.agent.ctx.root.llm.providerRetryPolicy('scripted')).toMatchObject({ retryableCodes: expect.arrayContaining(['RATE_LIMIT']) });
  });
  it('preserves validated neutral failure classification carried by ordinary adapter Errors', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([]);
    const source = Object.assign(adapterDiagnostic('RATE_LIMIT'), { failure: {
      message: diagnosticCredential, code: 'RATE_LIMIT', status: 429, providerRetryAfterMs: 400,
      requestId: diagnosticCredential,
    } });
    vi.spyOn(adapter, 'resolveModel').mockRejectedValue(source);
    const session = await create(f.binding, adapter);
    const error = await session.agent.ctx.root.llm.resolveModelInfo('scripted', 'offline-model').catch(error => error);
    expectSafeAdapterError(error); expect(error).toMatchObject({ code: 'RATE_LIMIT', failure: { status: 429, providerRetryAfterMs: 400 } });
  });
  it('preserves custom retry eligibility without publishing arbitrary provider error-code strings', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([]);
    vi.spyOn(adapter, 'providerRetryPolicy').mockReturnValue(resolveRetryPolicy({ mode: 'normal', maxRetries: 2, retryableCodes: [diagnosticCredential] }, 'offline'));
    vi.spyOn(adapter, 'resolveModel').mockRejectedValue(new LlmError(diagnosticCredential, diagnosticCredential, { cause: adapterDiagnostic() }));
    const session = await create(f.binding, adapter), llm = session.agent.ctx.root.llm;
    const error = await llm.resolveModelInfo('scripted', 'offline-model').catch(error => error);
    expectSafeAdapterError(error); const policy = llm.providerRetryPolicy('scripted');
    expect(policy).toMatchObject({ mode: 'normal', maxRetries: 2 });
    expect('retryableCodes' in policy && policy.retryableCodes.includes(error.code)).toBe(true);
    expect(JSON.stringify(policy)).not.toContain(diagnosticCredential);
    vi.spyOn(adapter, 'resolveModel').mockRejectedValue(new LlmError(diagnosticCredential, 'unconfigured-custom-code'));
    const other = await llm.resolveModelInfo('scripted', 'offline-model').catch(error => error);
    expect('retryableCodes' in policy && policy.retryableCodes.includes(other.code)).toBe(false);
  });
  it('preserves metadata, receivers, generation-bound preparation, signals and legitimate successful content', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([]), signal = new AbortController().signal;
    const policy = resolveRetryPolicy({ mode: 'normal', maxRetries: 3, retryableCodes: ['RATE_LIMIT'] }, 'offline');
    const metadata = { provider: 'scripted', id: 'offline-model', name: 'Display name', context: { contextWindow: 8192 }, defaultMaxTokens: 500,
      inputModalities: ['text'] as const, systemPromptUpdate: 'in-history' as const, toolUpdate: 'addition-only' as const };
    const pricing = { priceImages: vi.fn().mockReturnValue([]) };
    vi.spyOn(adapter, 'providerInfo').mockImplementation(function (this: ScriptedAdapter, provider) { expect(this).toBe(adapter); return { id: provider, name: 'Offline provider' }; });
    vi.spyOn(adapter, 'providerRetryPolicy').mockReturnValue(policy);
    vi.spyOn(adapter, 'imageRequestPricing').mockReturnValue(pricing);
    vi.spyOn(adapter, 'listModels').mockResolvedValue([metadata]);
    vi.spyOn(adapter, 'resolveModel').mockImplementation(async function (this: ScriptedAdapter, provider, model, receivedSignal) {
      expect(this).toBe(adapter); expect([provider, model, receivedSignal]).toEqual(['scripted', 'offline-model', signal]); return metadata;
    });
    const prepared = { model: metadata, stream: vi.fn().mockImplementation(function (this: unknown, request: GenerateOptions) {
      expect(this).toBe(prepared); expect(request.signal).toBe(signal);
      return (async function* () { for (const chunk of textResponse(diagnosticCredential)) yield chunk; })();
    }) };
    vi.spyOn(adapter, 'prepareCall').mockImplementation(async function (this: ScriptedAdapter, provider, model, receivedSignal) {
      expect(this).toBe(adapter); expect([provider, model, receivedSignal]).toEqual(['scripted', 'offline-model', signal]); return prepared;
    });
    const session = await create(f.binding, adapter), llm = session.agent.ctx.root.llm;
    expect(llm.listProviders()).toEqual([{ id: 'scripted', name: 'Offline provider' }]);
    expect(llm.providerRetryPolicy('scripted')).toEqual(policy); expect(await llm.listModels('scripted')).toEqual([{ provider: 'scripted', id: 'offline-model', name: 'Display name', inputModalities: ['text'] }]);
    expect(await llm.resolveModelInfo('scripted', 'offline-model', signal)).toEqual(metadata);
    expect(llm.imageRequestPricing('scripted', 'offline-model')!.priceImages([])).toEqual([]); expect(pricing.priceImages).toHaveBeenCalledOnce();
    const call = await llm.prepareCall({ provider: 'scripted', model: 'offline-model' }, signal);
    expect(call.config).toMatchObject({ maxTokens: 500 }); expect(call.context).toEqual({ contextWindow: 8192 }); expect(call.retryPolicy).toEqual(policy);
    const chunks: StreamChunk[] = []; for await (const chunk of call.stream({ ...call.config, messages: [], signal })) chunks.push(chunk);
    expect(chunks).toEqual(textResponse(diagnosticCredential)); expect(prepared.stream).toHaveBeenCalledOnce();
  });
});

describe('headless adapter data snapshots', () => {
  it.each(['text-delta', 'block-end'] as const)('materializes lazy %s data before dsh can observe its diagnostic getter', async type => {
    const f = await fixture(), source = adapterDiagnostic(), adapter = new ScriptedAdapter([]);
    vi.spyOn(adapter, 'stream').mockImplementation(async function* () {
      yield { type: 'block-start', index: 0, blockType: 'text' };
      if (type === 'text-delta') yield { type, index: 0, get text(): string { throw source; } };
      else yield { type, index: 0, block: { type: 'text', get text(): string { throw source; } } };
    });
    const session = await create(f.binding, adapter), errors: unknown[] = [];
    session.agent.ctx.on('agent/error', payload => { errors.push(payload.error); });
    await session.run('Exercise the lazy stream data failure.');
    expect(errors).toHaveLength(1); expect(errors).not.toContain(source); expectSafeAdapterError(errors[0]);
    expect(JSON.stringify(session.agent.session.snapshotEvents())).not.toContain(diagnosticCredential);
  });
  it('snapshots plain and nested successful chunks without changing legitimate content', async () => {
    const f = await fixture(), delta: StreamChunk & { type: 'text-delta' } = { type: 'text-delta', index: 0, text: diagnosticCredential };
    const usage = { inputTokens: 12, outputTokens: 8 }, response = { id: 'response-a', nested: { texts: [diagnosticCredential] } };
    const scripted: StreamChunk[] = [{ type: 'block-start', index: 0, blockType: 'text' }, delta,
      { type: 'block-end', index: 0, block: { type: 'text', text: diagnosticCredential } },
      { type: 'usage', usage }, { type: 'finish', reason: { kind: 'stop' }, replayState: { response, blocks: [{ nested: [diagnosticCredential] }] } }];
    const expected = structuredClone(scripted), session = await create(f.binding, new ScriptedAdapter([scripted]));
    const received: StreamChunk[] = [];
    for await (const chunk of session.agent.ctx.root.llm.stream({ provider: 'scripted', model: 'offline-model', messages: [] })) received.push(chunk);
    expect(received).toEqual(expected);
    for (let index = 0; index < scripted.length; index++) expect(received[index]).not.toBe(scripted[index]);
    expect((received[3] as { usage: unknown }).usage).not.toBe(usage);
    expect((received[4] as { replayState: { response: unknown } }).replayState.response).not.toBe(response);
    delta.text = 'changed'; usage.inputTokens = 999; response.nested.texts[0] = 'changed';
    expect(received).toEqual(expected);
  });
  it('converts uncloneable adapter stream data to a safe failure', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([]);
    vi.spyOn(adapter, 'stream').mockImplementation(async function* () {
      yield { type: 'finish', reason: { kind: 'stop' }, replayState: { response: { unsupported: () => diagnosticCredential }, blocks: [] } };
    });
    const session = await create(f.binding, adapter), received: StreamChunk[] = [];
    for await (const chunk of session.agent.ctx.root.llm.stream({ provider: 'scripted', model: 'offline-model', messages: [] })) received.push(chunk);
    expect(received.at(-1)).toMatchObject({ type: 'finish', reason: { kind: 'error', failure: { message: expect.stringMatching(/LLM adapter .* failed/) } } });
    expect(JSON.stringify(received)).not.toContain(diagnosticCredential);
  });
  it.each(['providerInfo', 'providerRetryPolicy', 'listModels', 'resolveModel', 'prepared.model', 'priceImages'] as const)(
    'materializes lazy %s metadata inside its guarded callback', async method => {
      const f = await fixture(), adapter = new ScriptedAdapter([]), source = adapterDiagnostic();
      const lazyName = { provider: 'scripted', id: 'offline-model', get name(): string { throw source; } };
      const lazyModel = { provider: 'scripted', id: 'offline-model', name: 'Offline model', context: { get contextWindow(): number { throw source; } } };
      if (method === 'providerInfo') vi.spyOn(adapter, 'providerInfo').mockReturnValue({ id: 'scripted', get name(): string { throw source; } });
      if (method === 'providerRetryPolicy') vi.spyOn(adapter, 'providerRetryPolicy').mockReturnValue({ mode: 'normal', maxRetries: 1,
        initialDelayMs: 10, maxDelayMs: 20, jitterRatio: 0, get retryableCodes(): readonly string[] { throw source; } });
      if (method === 'listModels') vi.spyOn(adapter, 'listModels').mockResolvedValue([lazyName]);
      if (method === 'resolveModel') vi.spyOn(adapter, 'resolveModel').mockResolvedValue(lazyModel);
      if (method === 'prepared.model') vi.spyOn(adapter, 'prepareCall').mockResolvedValue({ model: lazyModel, stream: adapter.stream.bind(adapter) });
      if (method === 'priceImages') vi.spyOn(adapter, 'imageRequestPricing').mockReturnValue({ priceImages: () => [{ visualTokens: 1, get text(): string { throw source; } }] });
      const error = await Promise.resolve().then<unknown>(async () => {
        const session = await create(f.binding, adapter), llm = session.agent.ctx.root.llm;
        if (method === 'listModels') return llm.listModels('scripted');
        if (method === 'resolveModel') return llm.resolveModelInfo('scripted', 'offline-model');
        if (method === 'prepared.model') return llm.prepareCall({ provider: 'scripted', model: 'offline-model' });
        if (method === 'priceImages') return llm.imageRequestPricing('scripted', 'offline-model')!.priceImages([])[0]!.text;
        return session;
      }).then(() => undefined, error => error);
      expect(error).not.toBe(source); expectSafeAdapterError(error);
    });
  it('snapshots image pricing data while preserving its callback receiver and successful text', async () => {
    const f = await fixture(), adapter = new ScriptedAdapter([]), price = { visualTokens: 12, text: diagnosticCredential };
    const pricing = { priceImages: vi.fn().mockImplementation(function (this: unknown) { expect(this).toBe(pricing); return [price]; }) };
    vi.spyOn(adapter, 'imageRequestPricing').mockReturnValue(pricing);
    const session = await create(f.binding, adapter), received = session.agent.ctx.root.llm.imageRequestPricing('scripted', 'offline-model')!.priceImages([]);
    expect(received).toEqual([price]); expect(received[0]).not.toBe(price);
    price.text = 'changed'; expect(received[0]!.text).toBe(diagnosticCredential);
  });
});
