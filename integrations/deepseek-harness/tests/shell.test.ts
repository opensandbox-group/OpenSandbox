// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { Context } from '@deepseek-ai/cordis';
import { Sandbox } from '@alibaba-group/opensandbox';
import type { ServerStreamEvent } from '@alibaba-group/opensandbox';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RemoteShell } from '../src/shell.js';
import type { RemoteShellOptions } from '../src/shell.js';
import type { BindingState, BoundSandbox } from '../src/types.js';
import { deferred, fakeSandbox } from './fixtures/sdk-facade.js';

function fixture(options: RemoteShellOptions = {}, id = 'sandbox-a') {
  const sdk = fakeSandbox(id);
  sdk.commands.getCommandStatus.mockImplementation(async id => ({ id, running: false, exitCode: 0 }));
  const ctx = new Context();
  let state: BindingState = 'open';
  const descriptor = Object.freeze({ version: 1 as const, sessionId: 'session-a', sandboxId: sdk.sandbox.id, remoteCwd: '/workspace' });
  const binding: BoundSandbox = Object.freeze({ descriptor, sandbox: sdk.sandbox,
    get state() { return state; },
    close: async () => { state = 'closed'; }, kill: async () => { state = 'killed'; }, toJSON: () => descriptor });
  const shell = new RemoteShell(ctx, { binding, ...options });
  return { ...sdk, ctx, binding, shell };
}
function controlled(sdk: ReturnType<typeof fakeSandbox>) {
  const items: (ServerStreamEvent | Error | null)[] = [];
  let wake = deferred<void>();
  sdk.commands.runStream.mockImplementation(async function* () {
    for (;;) {
      while (!items.length) await wake.promise;
      const item = items.shift();
      if (!items.length) wake = deferred<void>();
      if (item === null) return;
      if (item instanceof Error) throw item;
      if (item) yield item;
    }
  });
  return { send(item: ServerStreamEvent | Error | null) { items.push(item); wake.resolve(); } };
}
async function tick() { for (let i = 0; i < 15; i++) await Promise.resolve(); }
afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals(); });

describe('remote dsh shell', () => {
  it('publishes an actual live handle before command completion and separates streams', async () => {
    const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'echo hello' }));
    expect(h.status).toBe('running');
    flow.send({ type: 'init', text: 'cmd-a' });
    flow.send({ type: 'stdout', text: 'hello' }); flow.send({ type: 'stderr', text: 'warning' });
    await tick();
    expect(h.readOutput()).toEqual({ delta: 'hello\n[stderr]\nwarning', lossy: false });
    expect(h.readOutput().delta).toBe('');
    expect(h.observed.stdout.readFrom(0).text).toBe('hello');
    const result = h.result(); expect(h.result()).toBe(result);
    flow.send({ type: 'execution_complete' });
    await expect(h.done).resolves.toBeUndefined();
    expect(h.status).toBe('completed'); expect(h.kill()).toBe(false);
    expect(await result).toMatchObject({ exitCode: 0, signal: null, timedOut: false, aborted: false,
      stdout: { text: 'hello', truncated: false }, stderr: { text: 'warning', truncated: false } });
  });
  it('normal nonzero exit resolves rather than becoming an infrastructure rejection', async () => {
    const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'exit 7' }));
    flow.send({ type: 'init', text: 'cmd' });
    flow.send({ type: 'error', error: { ename: 'CommandExecError', evalue: '7' } });
    flow.send(null);
    expect(await h.result()).toMatchObject({ exitCode: 7, timedOut: false, aborted: false });
    expect(h.status).toBe('completed');
  });
  it('bounds both streams and exposes independent monotonically advancing readers', async () => {
    const f = fixture({ maxOutputBytes: 5 }); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'output', stdoutMaxBytes: 8 }));
    flow.send({ type: 'init', text: 'cmd' });
    flow.send({ type: 'stdout', text: 'abcdef世' }); flow.send({ type: 'stderr', text: 'abcdef界' });
    await tick();
    expect(h.observed.stdout.readFrom(0)).toEqual({ text: 'bcdef世', nextOffset: 9, lossy: true });
    expect(h.observed.stderr.readFrom(0)).toEqual({ text: 'ef界', nextOffset: 9, lossy: true });
    expect(h.readOutput().lossy).toBe(true);
    flow.send({ type: 'execution_complete' });
    expect((await h.result()).stdout.truncated).toBe(true);
  });
  it('never rejects done and safely rejects lazy result on infrastructure launch failure without retry', async () => {
    const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'launch' }));
    flow.send(new Error('private token and URL'));
    await expect(h.done).resolves.toBeUndefined();
    expect(h.status).toBe('killed');
    expect(h.readOutput().delta).toContain('unknown');
    expect(h.observed.stderr.readFrom(0).text).not.toContain('private token');
    await expect(h.result()).rejects.toMatchObject({ code: 'outcome_unknown' });
    expect(f.commands.runStream).toHaveBeenCalledTimes(1);
  });
  it('missing terminal event queries known ID but never claims complete output', async () => {
    const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'disconnect' }));
    flow.send({ type: 'init', text: 'known' }); flow.send({ type: 'stdout', text: 'partial' }); flow.send(null);
    await h.done;
    expect(f.commands.getCommandStatus).toHaveBeenCalledWith('known');
    expect(h.observed.stdout.readFrom(0).lossy).toBe(true);
    await expect(h.result()).rejects.toMatchObject({ code: 'output_incomplete', remoteState: 'stopped' });
  });
  it.each(['kill', 'abort'] as const)('accepts late %s after incomplete output with known running or unconfirmed status', async stop => {
    vi.useFakeTimers();
    for (const initial of ['running', 'unknown'] as const) {
      const f = fixture({ settlementTimeoutMs: 200 }); const flow = controlled(f); const abort = new AbortController();
      if (initial === 'running') f.commands.getCommandStatus.mockResolvedValueOnce({ id: 'known', running: true });
      else f.commands.getCommandStatus.mockRejectedValueOnce(new Error('status unavailable'));
      f.commands.getCommandStatus.mockResolvedValueOnce({ id: 'known', running: true }).mockResolvedValue({ id: 'known', running: false, exitCode: null });
      const h = await f.shell.execute(f.shell.resolve({ command: 'disconnect', onExpiry: 'none', signal: abort.signal }));
      flow.send({ type: 'init', text: 'known' }); flow.send({ type: 'stdout', text: 'partial' }); flow.send(null);
      await h.done; const failure = await h.result().catch(error => error);
      expect(failure).toMatchObject({ code: 'output_incomplete', remoteState: initial });
      if (stop === 'kill') expect(h.kill()).toBe(true); else abort.abort();
      await tick();
      expect(f.commands.interrupt).toHaveBeenCalledExactlyOnceWith('known');
      expect(h.kill()).toBe(false); abort.abort();
      await vi.advanceTimersByTimeAsync(100);
      expect(f.commands.getCommandStatus).toHaveBeenCalledTimes(3);
      expect(vi.getTimerCount()).toBe(0);
      await expect(h.result()).rejects.toBe(failure);
      expect(h.observed.stdout.readFrom(0)).toMatchObject({ text: 'partial', lossy: true });
      expect(f.commands.runStream).toHaveBeenCalledTimes(1);
      await f.shell.dispose(); expect(f.commands.interrupt).toHaveBeenCalledTimes(1);
    }
  });
  it('bounds late stop observation without changing an already rejected result', async () => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 20 }); const flow = controlled(f);
    f.commands.getCommandStatus.mockResolvedValueOnce({ id: 'known', running: true }).mockReturnValue(new Promise(() => {}));
    f.commands.interrupt.mockReturnValue(new Promise(() => {}));
    const h = await f.shell.execute(f.shell.resolve({ command: 'disconnect', onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'known' }); flow.send(null); await h.done;
    const failure = await h.result().catch(error => error);
    expect(h.kill()).toBe(true); await vi.advanceTimersByTimeAsync(20);
    expect(vi.getTimerCount()).toBe(0); expect(h.kill()).toBe(false);
    expect(f.commands.interrupt).toHaveBeenCalledTimes(1); expect(f.commands.getCommandStatus).toHaveBeenCalledTimes(2);
    await expect(h.result()).rejects.toBe(failure); expect(failure).toMatchObject({ code: 'output_incomplete', remoteState: 'running' });
  });
  it('does not accept a late stop after incomplete output from a confirmed stopped command', async () => {
    const f = fixture(); const flow = controlled(f); const abort = new AbortController();
    const h = await f.shell.execute(f.shell.resolve({ command: 'disconnect', onExpiry: 'none', signal: abort.signal }));
    flow.send({ type: 'init', text: 'known' }); flow.send(null); await h.done;
    await expect(h.result()).rejects.toMatchObject({ code: 'output_incomplete', remoteState: 'stopped' });
    abort.abort(); expect(h.kill()).toBe(false); await f.shell.dispose();
    expect(f.commands.interrupt).not.toHaveBeenCalled();
  });
  it('pre-abort performs no launch or stdin mutation', async () => {
    const f = fixture(); const abort = new AbortController(); abort.abort();
    await expect(f.shell.execute(f.shell.resolve({ command: 'no', signal: abort.signal, stdin: 'input' }))).rejects.toMatchObject({ code: 'aborted' });
    expect(f.commands.runStream).not.toHaveBeenCalled(); expect(f.files.createDirectories).not.toHaveBeenCalled();
  });
  it.each(['timeout', 'abort'] as const)('records %s as first cause and explicitly interrupts despite an unresponsive SSE', async first => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 20 }); const flow = controlled(f);
    const abort = new AbortController();
    const h = await f.shell.execute(f.shell.resolve({ command: 'sleep', timeoutMs: 10, signal: abort.signal }));
    flow.send({ type: 'init', text: 'known' }); await tick();
    if (first === 'abort') abort.abort();
    await vi.advanceTimersByTimeAsync(10);
    if (first === 'timeout') abort.abort();
    await tick();
    expect(f.commands.interrupt).toHaveBeenCalledTimes(1);
    expect(f.commands.getCommandStatus).toHaveBeenCalledWith('known');
    expect(await h.result()).toMatchObject({ timedOut: first === 'timeout', aborted: first === 'abort', timeoutMs: 10 });
    expect(h.status).toBe('killed');
    expect((await h.result()).stdout.truncated).toBe(true);
  });
  it('onExpiry none and a caller-only wait timeout keep the process live', async () => {
    vi.useFakeTimers(); const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'sleep', timeoutMs: 5, onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'known' }); await tick();
    expect(f.commands.runStream.mock.calls[0]?.[1]).not.toHaveProperty('timeoutSeconds');
    const waiting = Promise.race([h.done.then(() => 'done'), new Promise(resolve => setTimeout(() => resolve('wait-only'), 10))]);
    await vi.advanceTimersByTimeAsync(100);
    expect(await waiting).toBe('wait-only'); expect(h.status).toBe('running'); expect(f.commands.interrupt).not.toHaveBeenCalled();
    expect(h.kill()).toBe(true); expect(h.kill()).toBe(false);
    await h.done;
  });
  it('settles locally when stop/status hang without claiming remote termination', async () => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 15 }); const flow = controlled(f);
    f.commands.interrupt.mockReturnValue(new Promise(() => {}));
    f.commands.getCommandStatus.mockReturnValue(new Promise(() => {}));
    const h = await f.shell.execute(f.shell.resolve({ command: 'sleep', onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'known' }); await tick();
    expect(h.kill()).toBe(true); expect(h.status).toBe('running');
    await vi.advanceTimersByTimeAsync(15); await h.done;
    await expect(h.result()).rejects.toMatchObject({ code: 'termination_unknown', commandId: 'known', remoteState: 'unknown' });
    expect(h.kill()).toBe(false);
  });
  it('honors late init after cancelled unknown launch without retrying', async () => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 10 }); const flow = controlled(f);
    const abort = new AbortController();
    const h = await f.shell.execute(f.shell.resolve({ command: 'late', onExpiry: 'none', signal: abort.signal, stdin: 'bytes' }));
    abort.abort(); await vi.advanceTimersByTimeAsync(10); await h.done;
    await expect(h.result()).rejects.toMatchObject({ code: 'outcome_unknown' });
    expect(f.files.deleteDirectories).not.toHaveBeenCalled();
    flow.send({ type: 'init', text: 'late-id' }); await tick();
    expect(f.commands.interrupt).toHaveBeenCalledWith('late-id');
    expect(f.commands.getCommandStatus).toHaveBeenCalledWith('late-id');
    await tick();
    expect(f.files.deleteDirectories).toHaveBeenCalledTimes(1);
    expect(f.commands.runStream).toHaveBeenCalledTimes(1);
  });
  it('carries cancellation through pending preparation and never launches late', async () => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 10 }); const write = deferred<void>();
    f.files.writeFiles.mockReturnValue(write.promise);
    const abort = new AbortController();
    const preparing = f.shell.execute(f.shell.resolve({ command: 'never', stdin: 'input', signal: abort.signal }));
    await tick(); abort.abort();
    await expect(preparing).rejects.toMatchObject({ code: 'aborted' });
    expect(f.commands.runStream).not.toHaveBeenCalled(); expect(f.files.deleteDirectories).not.toHaveBeenCalled();
    write.resolve(); await tick();
    expect(f.commands.runStream).not.toHaveBeenCalled();
    expect(f.files.deleteDirectories).toHaveBeenCalledTimes(1);
  });
  it('returns a no-output timed-out handle when the preparation deadline wins', async () => {
    vi.useFakeTimers(); const f = fixture(); const create = deferred<void>();
    f.files.createDirectories.mockReturnValue(create.promise);
    const abort = new AbortController();
    const preparing = f.shell.execute(f.shell.resolve({ command: 'never', stdin: 'input', timeoutMs: 10, signal: abort.signal }));
    await vi.advanceTimersByTimeAsync(10); abort.abort();
    const h = await preparing;
    expect(await h.result()).toMatchObject({ timedOut: true, aborted: false, stdout: { text: '', truncated: false }, stderr: { text: '', truncated: false } });
    create.resolve(); await tick(); expect(f.commands.runStream).not.toHaveBeenCalled();
  });
  it('disposal reports unconfirmed termination after an independently settled handle', async () => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 10 }); const flow = controlled(f);
    f.commands.getCommandStatus.mockResolvedValue({ id: 'known', running: true });
    const h = await f.shell.execute(f.shell.resolve({ command: 'sleep', onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'known' }); await tick();
    h.kill(); await vi.advanceTimersByTimeAsync(10); await h.done;
    const disposing = f.shell.dispose();
    const observed = disposing.then(() => ({ code: 'unexpected_success' }), error => error);
    await vi.advanceTimersByTimeAsync(10);
    expect(await observed).toMatchObject({ code: 'termination_unknown', remoteState: 'running' });
  });
  it('composition disposal stops and joins live handles and refuses later execution', async () => {
    const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'sleep', onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'known' }); await tick();
    await f.shell.dispose(); await h.done;
    expect(f.commands.interrupt).toHaveBeenCalledWith('known');
    await expect(f.shell.execute(f.shell.resolve({ command: 'no' }))).rejects.toMatchObject({ code: 'disposed' });
  });
  it('stages exact finite stdin as bytes and passes user command/path/env as data', async () => {
    const f = fixture(); const flow = controlled(f);
    const input = 'x\u0000世界\n$(touch bad)'; const command = 'cat; echo "$X"';
    const h = await f.shell.execute(f.shell.resolve({ command, stdin: input,
      env: { X: 'literal;$(bad)', DSH_SESSION_ID: 'wrong', DSH_STALE: 'stale' }, dshEnv: { DSH_SESSION_ID: 'session-a' } }));
    const directory = f.files.createDirectories.mock.calls[0]?.[0]?.[0];
    expect(directory).toMatchObject({ mode: 700 });
    const staged = f.files.writeFiles.mock.calls[0]?.[0]?.[0];
    expect(staged?.path).toBe(`${directory?.path}/stdin`); expect(staged?.mode).toBe(600);
    expect(staged?.data).toEqual(Buffer.from(input, 'utf8'));
    const [argv, options] = f.commands.runStream.mock.calls[0]!;
    expect(Array.isArray(argv)).toBe(true); expect((argv as string[])[0]).toBe('python3');
    expect(argv).toContain(command); expect(argv).toContain(staged?.path);
    expect((argv as string[])[2]).not.toContain(command);
    expect(options).toMatchObject({ background: false, workingDirectory: '/', envs: { X: 'literal;$(bad)', DSH_SESSION_ID: 'session-a' } });
    expect(options?.envs).not.toHaveProperty('DSH_STALE');
    expect(options?.envs).not.toHaveProperty('OPEN_SANDBOX_API_KEY');
    flow.send({ type: 'init', text: 'known' }); flow.send({ type: 'execution_complete' }); await h.done; await tick();
    expect(f.files.deleteDirectories).toHaveBeenCalledWith([directory?.path]);
  });
  it.each(['read-only', 'workspace-write'] as const)('rejects unsupported %s policy without remote calls', async mode => {
    const f = fixture();
    expect(() => f.shell.resolve({ command: 'no', sandboxPolicy: { mode, workspaceRoot: '/workspace' } })).toThrow(/policy/i);
    expect(f.commands.runStream).not.toHaveBeenCalled();
  });

  it('binds same-path sessions to their own immutable sandbox transport', async () => {
    const a = fixture({}, 'sandbox-a'); const b = fixture({}, 'sandbox-b');
    const ha = await a.shell.execute(a.shell.resolve({ command: 'cat same.txt', stdin: 'A' }));
    const hb = await b.shell.execute(b.shell.resolve({ command: 'cat same.txt', stdin: 'B' }));
    await Promise.all([ha.done, hb.done]);
    expect(a.files.writeFiles.mock.calls[0]?.[0]?.[0]?.data).toEqual(Buffer.from('A'));
    expect(b.files.writeFiles.mock.calls[0]?.[0]?.[0]?.data).toEqual(Buffer.from('B'));
    expect(a.commands.runStream.mock.calls[0]?.[1]?.workingDirectory).toBe('/');
    expect(a.commands.runStream.mock.calls[0]?.[0]?.[6]).toBe('/workspace');
    expect(b.commands.runStream.mock.calls[0]?.[1]?.workingDirectory).toBe('/');
    expect(b.commands.runStream.mock.calls[0]?.[0]?.[6]).toBe('/workspace');
    expect(a.commands.runStream).toHaveBeenCalledTimes(1); expect(b.commands.runStream).toHaveBeenCalledTimes(1);
  });
  it('does not arm an expiry timer during none-policy stdin preparation', async () => {
    vi.useFakeTimers(); const f = fixture(); const creation = deferred<void>(); const flow = controlled(f);
    f.files.createDirectories.mockReturnValue(creation.promise);
    const preparing = f.shell.execute(f.shell.resolve({ command: 'cat', stdin: 'data', timeoutMs: 5, onExpiry: 'none' }));
    await vi.advanceTimersByTimeAsync(100);
    expect(f.commands.runStream).not.toHaveBeenCalled(); expect(vi.getTimerCount()).toBe(0);
    creation.resolve(); const h = await preparing;
    flow.send({ type: 'init', text: 'known' }); flow.send({ type: 'execution_complete' }); await h.done;
    expect(await h.result()).toMatchObject({ timedOut: false, aborted: false, timeoutMs: 5 });
    expect(vi.getTimerCount()).toBe(0);
  });
  it('retains unknown stdin mutations without retries or launching', async () => {
    const f = fixture(); f.files.writeFiles.mockRejectedValue(new Error('secret URL'));
    const error = await f.shell.execute(f.shell.resolve({ command: 'no', stdin: 'input' })).catch(error => error);
    expect(error).toMatchObject({ code: 'preparation_unknown' }); expect(String(error)).not.toContain('secret URL');
    expect(f.files.writeFiles).toHaveBeenCalledTimes(1); expect(f.commands.runStream).not.toHaveBeenCalled();
    expect(f.files.deleteDirectories).not.toHaveBeenCalled();
    await expect(f.shell.dispose()).rejects.toMatchObject({ code: 'preparation_unknown' });
  });
  it('polls running status until stop confirmation and clears every local timer', async () => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 200 }); const flow = controlled(f);
    f.commands.getCommandStatus.mockResolvedValueOnce({ id: 'known', running: true }).mockResolvedValue({ id: 'known', running: false, exitCode: null });
    f.commands.interrupt.mockRejectedValue(new Error('interrupt acknowledgement lost'));
    const h = await f.shell.execute(f.shell.resolve({ command: 'sleep', onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'known' }); await tick(); h.kill(); await tick();
    expect(h.status).toBe('running');
    await vi.advanceTimersByTimeAsync(100); await h.done;
    expect(f.commands.getCommandStatus).toHaveBeenCalledTimes(2);
    expect(vi.getTimerCount()).toBe(0); expect((await h.result()).stdout.truncated).toBe(true);
  });
  it('rejects an incompatible resolved policy at execution time', async () => {
    const f = fixture(); const spec = f.shell.resolve({ command: 'no' });
    spec.sandboxPolicy = { mode: 'read-only', workspaceRoot: '/workspace' };
    await expect(f.shell.execute(spec)).rejects.toMatchObject({ code: 'unsupported_policy' });
    expect(f.commands.runStream).not.toHaveBeenCalled();
  });
  it('Cordis plugin disposal invokes remote ownership cleanup', async () => {
    const f = fixture(); const ctx = new Context(); const flow = controlled(f);
    const fiber = ctx.plugin(RemoteShell, { binding: f.binding }); await fiber;
    const h = await ctx.shell.execute(ctx.shell.resolve({ command: 'sleep', onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'known' }); await tick();
    await fiber.dispose(); await h.done;
    expect(f.commands.interrupt).toHaveBeenCalledWith('known'); expect(ctx.get('shell')).toBeUndefined();
  });
  it('uses published SDK SSE/argv contracts and independently cancels after response headers', async () => {
    const requests: { url: string; method: string; body: unknown }[] = [];
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    let stopped = false;
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : 'GET'); requests.push({ url, method, body: init?.body ? JSON.parse(String(init.body)) : undefined });
      if (url.includes('/endpoints/')) return Response.json({ endpoint: 'offline.invalid/real-sdk' });
      if (url.endsWith('/command') && method === 'POST') return new Response(new ReadableStream<Uint8Array>({ start(c) { controller = c; } }), { headers: { 'content-type': 'text/event-stream' } });
      if (new URL(url).pathname.endsWith('/command') && method === 'DELETE') { stopped = true; return new Response(null, { status: 204 }); }
      if (url.includes('/command/status/')) return Response.json({ id: 'sdk-command', running: !stopped, exit_code: null });
      throw new Error('unexpected offline request');
    });
    const sandbox = await Sandbox.connect({ sandboxId: 'real-sdk', skipHealthCheck: true,
      connectionConfig: { domain: 'offline.invalid', disableMetrics: true } });
    const descriptor = Object.freeze({ version: 1 as const, sandboxId: sandbox.id, sessionId: 'session-sdk', remoteCwd: '/same' });
    const binding: BoundSandbox = Object.freeze({ descriptor, sandbox, state: 'open' as const,
      close: () => sandbox.close(), kill: () => sandbox.kill(), toJSON: () => descriptor });
    const shell = new RemoteShell(new Context(), { binding, settlementTimeoutMs: 500 });
    const abort = new AbortController(); const h = await shell.execute(shell.resolve({ command: 'cat "path with space"', onExpiry: 'none', signal: abort.signal }));
    await tick();
    const send = (event: ServerStreamEvent) => controller.enqueue(Buffer.from(`data: ${JSON.stringify(event)}\n\n`));
    send({ type: 'init', text: 'sdk-command' }); send({ type: 'stdout', text: '世界😀' }); send({ type: 'stderr', text: 'warning' }); await tick();
    expect(h.observed.stdout.readFrom(0).text).toBe('世界😀');
    const post = requests.find(r => r.method === 'POST');
    expect(post?.body).toMatchObject({ argv: ['python3', '-c', expect.any(String), '', 'cat "path with space"', '[]', '/same'], cwd: '/', background: false });
    expect(post?.body).not.toHaveProperty('timeout');
    abort.abort(); await h.done;
    expect(await h.result()).toMatchObject({ aborted: true, timedOut: false, stdout: { text: '世界😀', truncated: true } });
    expect(requests.filter(r => r.method === 'POST')).toHaveLength(1);
    expect(requests.some(r => r.method === 'DELETE' && r.url.includes('id=sdk-command'))).toBe(true);
    expect(requests.some(r => r.url.includes('/command/status/sdk-command'))).toBe(true);
    controller.close(); await tick(); await shell.dispose(); await sandbox.close();
  });


  it('preserves every valid ordinary environment key, including prototype-named keys', async () => {
    const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'env', env: JSON.parse('{"__proto__":"literal","constructor":"also literal"}') as Record<string, string> }));
    const envs = f.commands.runStream.mock.calls[0]?.[1]?.envs;
    expect(Object.hasOwn(envs!, '__proto__')).toBe(true); expect(envs?.['__proto__']).toBe('literal');
    expect(envs?.['constructor']).toBe('also literal');
    flow.send({ type: 'execution_complete' }); await h.done;
  });
  it('replaces an incomplete-stream wait bound when cancellation starts', async () => {
    vi.useFakeTimers(); const f = fixture({ settlementTimeoutMs: 20 }); const flow = controlled(f);
    f.commands.getCommandStatus.mockReturnValue(new Promise(() => {}));
    const abort = new AbortController(); const h = await f.shell.execute(f.shell.resolve({ command: 'sleep', onExpiry: 'none', signal: abort.signal }));
    flow.send({ type: 'init', text: 'known' }); flow.send(null); await tick();
    await vi.advanceTimersByTimeAsync(10); abort.abort();
    await vi.advanceTimersByTimeAsync(10); expect(h.status).toBe('running');
    await vi.advanceTimersByTimeAsync(10); await h.done;
    await expect(h.result()).rejects.toMatchObject({ code: 'termination_unknown', aborted: true });
    expect(vi.getTimerCount()).toBe(0);
  });


  it.each([false, true])('published SDK numeric terminal error resolves with delayed-open stream=%s', async leaveOpen => {
    let streamController!: ReadableStreamDefaultController<Uint8Array>;
    let streamCancelled = false;
    const requests: string[] = [];
    const events: ServerStreamEvent[] = [{ type: 'init', text: 'nonzero-7' }, { type: 'stdout', text: 'before\n' },
      { type: 'error', error: { ename: 'CommandExecError', evalue: '7', traceback: ['exit status 7'] } }];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = input instanceof Request ? input.url : String(input);
      const method = init?.method ?? (input instanceof Request ? input.method : 'GET'); requests.push(`${method} ${new URL(url).pathname}`);
      if (url.includes('/endpoints/')) return Response.json({ endpoint: 'offline.invalid/nonzero' });
      if (new URL(url).pathname.endsWith('/command') && method === 'POST') return new Response(new ReadableStream<Uint8Array>({ start(c) {
        streamController = c;
        for (const event of events) c.enqueue(Buffer.from(`data: ${JSON.stringify(event)}\n\n`));
        if (!leaveOpen) c.close();
      }, cancel() { streamCancelled = true; } }), { headers: { 'content-type': 'text/event-stream' } });
      if (url.includes('/command/status/')) return Response.json({ id: 'nonzero-7', running: false, exit_code: 7 });
      if (method === 'DELETE') return new Response(null, { status: 204 });
      throw new Error('unexpected offline request');
    });
    const sandbox = await Sandbox.connect({ sandboxId: 'nonzero', skipHealthCheck: true,
      connectionConfig: { domain: 'offline.invalid', disableMetrics: true } });
    const descriptor = Object.freeze({ version: 1 as const, sandboxId: sandbox.id, sessionId: 'nonzero', remoteCwd: '/work' });
    const binding: BoundSandbox = Object.freeze({ descriptor, sandbox, state: 'open' as const,
      close: () => sandbox.close(), kill: () => sandbox.kill(), toJSON: () => descriptor });
    const shell = new RemoteShell(new Context(), { binding, settlementTimeoutMs: 20 });
    try {
      const h = await shell.execute(shell.resolve({ command: 'exit 7', timeoutMs: 10 }));
      await h.done;
      const projected = await h.result().then(value => ({ kind: 'resolved', value }), error => ({ kind: 'rejected', code: error.code }));
      expect(projected).toMatchObject({ kind: 'resolved', value: { exitCode: 7, timedOut: false, aborted: false,
        stdout: { text: 'before\n', truncated: false } } });
      expect(h.status).toBe('completed'); expect(h.exitCode).toBe(7);
      await new Promise(resolve => setTimeout(resolve, 15));
      expect(h.status).toBe('completed'); expect(requests.some(r => r.startsWith('DELETE'))).toBe(false);
      expect(requests.some(r => r.includes('/command/status/'))).toBe(false);
    } finally { if (leaveOpen && !streamCancelled) streamController.close(); await shell.dispose(); await sandbox.close(); }
  });
  it('nonnumeric terminal start errors stay sanitized infrastructure failures', async () => {
    const f = fixture(); const flow = controlled(f);
    const h = await f.shell.execute(f.shell.resolve({ command: 'missing-runner', onExpiry: 'none' }));
    flow.send({ type: 'init', text: 'failed-start' });
    flow.send({ type: 'error', error: { ename: 'CommandExecError', evalue: 'private-token executable start failed', traceback: ['private-token'] } });
    flow.send(null); await h.done;
    expect(h.status).toBe('killed');
    await expect(h.result()).rejects.toMatchObject({ code: 'runner_failed', remoteState: 'stopped' });
    expect(h.observed.stderr.readFrom(0).text).not.toContain('private-token');
    expect(f.commands.getCommandStatus).not.toHaveBeenCalled();
  });


  it('keeps explicit environment values out of execd command argv and status diagnostics', async () => {
    const f = fixture(); const flow = controlled(f);
    const token = 'synthetic-env-secret-review-only'; const managed = 'synthetic-managed-secret-review-only';
    const h = await f.shell.execute(f.shell.resolve({ command: 'printf "%s|%s|%s" "$TOKEN" "$DSH_SESSION_ID" "${DSH_STALE-unset}"', workdir: '/tmp',
      env: { TOKEN: token, DSH_SESSION_ID: 'discarded-ordinary-managed-value' }, dshEnv: { DSH_SESSION_ID: managed } }));
    const [command, options] = f.commands.runStream.mock.calls[0]!;
    if (!Array.isArray(command)) throw new Error('Expected fixed wrapper argv');
    // Execd commandContent/log/status records are JSON(argv), not environment assignments.
    const commandDiagnostics = JSON.stringify(command);
    expect(commandDiagnostics).not.toContain(token); expect(commandDiagnostics).not.toContain(managed);
    expect(commandDiagnostics).not.toContain('discarded-ordinary-managed-value');
    expect(JSON.parse(command[5]!)).toEqual(['DSH_SESSION_ID']);
    expect(options?.envs).toMatchObject({ TOKEN: token, DSH_SESSION_ID: managed });
    // This test alone runs the exact fixed wrapper locally against synthetic environment data.
    const run = spawnSync(command[0]!, command.slice(1), { cwd: '/tmp', env: {
      PATH: process.env.PATH ?? '/usr/bin:/bin', DSH_STALE: 'ambient-stale', ...options?.envs,
    } });
    expect(run.error).toBeUndefined(); expect(run.status).toBe(0);
    expect(run.stdout.toString()).toBe(`${token}|${managed}|unset`);
    flow.send({ type: 'execution_complete' }); await h.done;
  });


  it('establishes literal dollar-expression cwd with a distinct existing expansion target', async () => {
    const root = mkdtempSync(join(tmpdir(), 'dsh-literal-cwd-'));
    const literal = join(root, '$PROJECT'); const expansion = join(root, 'different-project');
    mkdirSync(literal); mkdirSync(expansion); writeFileSync(join(literal, 'sentinel'), 'literal'); writeFileSync(join(expansion, 'sentinel'), 'expanded');
    const f = fixture(); const flow = controlled(f);
    try {
      const h = await f.shell.execute(f.shell.resolve({ command: 'cat sentinel; printf "\n%s" "$PWD"', workdir: literal, env: { PROJECT: 'different-project' } }));
      const [command, options] = f.commands.runStream.mock.calls[0]!;
      if (!Array.isArray(command)) throw new Error('Expected fixed wrapper argv');
      expect(options?.workingDirectory).toBe('/'); expect(command[6]).toBe(literal);
      // The SDK-safe launch cwd is /; only the fixed wrapper establishes the literal target.
      const run = spawnSync(command[0]!, command.slice(1), { cwd: '/', env: { PATH: process.env.PATH ?? '/usr/bin:/bin', ...options?.envs } });
      expect(run.error).toBeUndefined(); expect(run.status).toBe(0);
      expect(run.stdout.toString()).toBe(`literal\n${literal}`);
      flow.send({ type: 'execution_complete' }); await h.done;
    } finally { rmSync(root, { recursive: true }); }
  });

  it('rejects other session policies, invalid paths/budgets/env and closed binding', async () => {
    const f = fixture({ maxStdinBytes: 3, maxTimeoutMs: 100 });
    expect(f.shell.resolve({ command: 'yes', timeoutMs: 1000 }).timeoutMs).toBe(100);
    expect(() => f.shell.resolve({ command: 'no', workdir: 'host-relative' })).toThrow();
    expect(() => f.shell.resolve({ command: 'no', stdoutMaxBytes: NaN })).toThrow();
    expect(() => f.shell.resolve({ command: 'no', stdin: '世界' })).toThrow();
    expect(() => f.shell.resolve({ command: 'no', env: { 'bad-key': 'x' } })).toThrow();
    expect(() => f.shell.resolve({ command: 'no', sandboxPolicy: { mode: 'danger-full-access', workspaceRoot: '/workspace', sessionId: 'other' as never } })).toThrow(/session/i);
    await f.binding.close();
    await expect(f.shell.execute(f.shell.resolve({ command: 'no' }))).rejects.toMatchObject({ code: 'binding_closed' });
  });
});
