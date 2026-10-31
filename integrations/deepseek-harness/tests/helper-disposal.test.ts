// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { spawn } from 'node:child_process';
import { readFile, stat, writeFile } from 'node:fs/promises';
import { inspect } from 'node:util';
import { Context } from '@deepseek-ai/cordis';
import { afterEach, describe, expect, it } from 'vitest';
import type { ServerStreamEvent } from '@alibaba-group/opensandbox';
import { RemoteFileSystem } from '../src/filesystem.js';
import { RemoteFsError } from '../src/helper-client.js';
import { helperSdk } from './fixtures/helper-sdk.js';
import { deferred } from './fixtures/sdk-facade.js';

const cleanups: (() => Promise<void>)[] = [];
afterEach(async () => { for (const cleanup of cleanups.splice(0).reverse()) await cleanup(); });
async function fixture() {
  const sdk = await helperSdk();
  const fs = new RemoteFileSystem(new Context(), { binding: sdk.binding, timeoutMs: 2_000, settlementTimeoutMs: 80 });
  cleanups.push(async () => { await fs.dispose().catch(() => undefined); await sdk.cleanup(); });
  const target = await fs.resolve('note.txt'); await writeFile(sdk.local('/workspace/note.txt'), 'original');
  return { ...sdk, fs, target };
}
async function waitForFile(path: string) {
  for (let attempt = 0; attempt < 400; attempt++) {
    try { await stat(path); return; } catch { await new Promise(resolve => setTimeout(resolve, 5)); }
  }
  throw new Error('Fixture helper did not reserve its response.');
}
async function holdTarget(f: Awaited<ReturnType<typeof fixture>>) {
  const asset = f.uploads.find(upload => upload.path.endsWith('/fs_helper.py'))!;
  const holder = spawn('python3', ['-c', [
    'import importlib.util,sys',
    'spec=importlib.util.spec_from_file_location("helper",sys.argv[1])',
    'helper=importlib.util.module_from_spec(spec); spec.loader.exec_module(helper)',
    'with helper._target_lock({"path":sys.argv[2],"displayPath":sys.argv[2]},{}):',
    ' print("locked",flush=True)',
    ' sys.stdin.read()',
  ].join('\n'), f.local(asset.path), f.local('/workspace/note.txt')]);
  const exited = new Promise<void>(resolve => holder.once('close', () => resolve()));
  cleanups.push(async () => { holder.kill('SIGKILL'); await exited; });
  await new Promise<void>((resolve, reject) => {
    holder.stdout.once('data', () => resolve());
    holder.once('error', reject); holder.once('exit', () => reject(new Error('Fixture lock holder exited before readiness.')));
  });
  return async () => { holder.stdin.end(); await exited; };
}
function observeLaunch(f: Awaited<ReturnType<typeof fixture>>) {
  const launched = deferred<string>();
  f.controls.afterLaunch = async (argv, id) => {
    if (!argv[1]?.endsWith('/fs_helper.py')) return;
    // Response reservation proves Python installed its cancellation handlers before interrupt.
    await waitForFile(f.local(argv[3]!)); launched.resolve(id);
  };
  return launched.promise;
}

// Real helper processes contend on the production lock implementation, not a simulated delay.
describe('filesystem command ownership after unknown settlement', () => {
  it.each(['pending', 'settled unknown'] as const)('rejects disposal of a %s write when interrupt is ineffective', async state => {
    const f = await fixture(), release = await holdTarget(f), launched = observeLaunch(f);
    const count = f.requestCount(), abort = new AbortController();
    f.commands.interrupt.mockResolvedValue(undefined);
    const writing = f.fs.writeText(f.target, 'after-disposal', undefined, abort.signal).catch(error => error);
    const commandId = await launched;
    if (state === 'settled unknown') {
      abort.abort();
      expect(await writing).toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown', remoteState: 'unknown', commandId });
    }
    const deleted = f.files.deleteDirectories.mock.calls.length;
    const result = await f.fs.dispose().then(() => undefined, error => error);
    expect(await readFile(f.local('/workspace/note.txt'), 'utf8')).toBe('original');
    await release();
    await expect.poll(async () => readFile(f.local('/workspace/note.txt'), 'utf8')).toBe('after-disposal');
    expect(result).toBeInstanceOf(RemoteFsError);
    expect(result).toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown', remoteState: 'unknown', commandId });
    expect(await writing).toMatchObject({ outcome: 'unknown', commandId });
    expect(f.commands.interrupt).toHaveBeenCalledExactlyOnceWith(commandId);
    expect(f.files.deleteDirectories).toHaveBeenCalledTimes(deleted);
    await expect(f.fs.writeText(f.target, 'retry')).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(f.requestCount()).toBe(count + 1);
    expect(f.sandbox.kill).not.toHaveBeenCalled(); expect(f.sandbox.close).not.toHaveBeenCalled();
  });

  it('fulfills disposal only after an effective interrupt confirms prepublication cancellation', async () => {
    const f = await fixture(), release = await holdTarget(f), launched = observeLaunch(f);
    const writing = f.fs.writeText(f.target, 'never').catch(error => error), commandId = await launched;
    await expect(f.fs.dispose()).resolves.toBeUndefined();
    expect(await writing).toMatchObject({ code: 'FS_ABORTED', remoteState: 'stopped', commandId });
    expect(f.commands.interrupt).toHaveBeenCalledExactlyOnceWith(commandId);
    await release(); expect(await readFile(f.local('/workspace/note.txt'), 'utf8')).toBe('original');
    expect(await f.artifactRoots()).toEqual([]);
  });
});

function helperStream(f: Awaited<ReturnType<typeof fixture>>, events: () => AsyncIterable<ServerStreamEvent>) {
  const original = f.commands.runStream.getMockImplementation()!;
  f.commands.runStream.mockImplementation(async function* (argv, options, signal) {
    if (Array.isArray(argv) && argv[1]?.endsWith('/fs_helper.py')) yield* events();
    else yield* original(argv, options, signal);
  });
}

// Transport-edge controls isolate stop evidence from the already-settled request outcome.
describe('bounded disposal stop evidence', () => {
  it.each(['ended', 'invalid ID'] as const)('retains an unknown %s stream with no usable command ID', async kind => {
    const f = await fixture();
    helperStream(f, async function* () {
      if (kind === 'invalid ID') yield { type: 'init', text: 'private https://credentials.invalid/token' };
    });
    const writing = await f.fs.writeText(f.target, 'secret-content').catch(error => error);
    expect(writing).toMatchObject({ outcome: 'unknown', remoteState: 'unknown', commandId: undefined });
    const disposed = await f.fs.dispose().catch(error => error);
    expect(disposed).toMatchObject({ outcome: 'unknown', remoteState: 'unknown', commandId: undefined });
    expect(inspect(disposed, { depth: 20 })).not.toMatch(/secret-content|credentials.invalid/);
    expect(f.commands.interrupt).not.toHaveBeenCalled(); expect(f.commands.getCommandStatus).not.toHaveBeenCalled();
  });

  it.each(['running', 'mismatched', 'failed', 'hung'] as const)('bounds %s status and exposes only sanitized diagnostics', async mode => {
    const f = await fixture(), secret = 'private-sdk-diagnostic';
    helperStream(f, async function* () { yield { type: 'init', text: 'owned-command' }; });
    f.commands.interrupt.mockImplementation(() => new Promise(() => undefined));
    f.commands.getCommandStatus.mockImplementation(async () => {
      if (mode === 'hung') return new Promise(() => undefined);
      if (mode === 'failed') throw new Error(secret);
      return { id: mode === 'mismatched' ? 'other-command' : 'owned-command', running: mode !== 'mismatched', exitCode: 0 };
    });
    const writing = await f.fs.writeText(f.target, secret).catch(error => error);
    expect(writing).toMatchObject({ outcome: 'unknown', remoteState: 'unknown', commandId: 'owned-command' });
    const start = Date.now(), disposed = await f.fs.dispose().catch(error => error);
    expect(Date.now() - start).toBeLessThan(500);
    expect(disposed).toBeInstanceOf(RemoteFsError);
    expect(disposed).toMatchObject({ outcome: 'unknown', remoteState: 'unknown', commandId: 'owned-command' });
    expect(inspect(disposed, { depth: 20 }) + JSON.stringify(disposed)).not.toContain(secret);
    expect(f.commands.interrupt).toHaveBeenCalledExactlyOnceWith('owned-command');
    const polls = f.commands.getCommandStatus.mock.calls.length;
    await new Promise(resolve => setTimeout(resolve, 120));
    expect(f.commands.getCommandStatus).toHaveBeenCalledTimes(polls);
  });

  it.each(['terminal', 'status'] as const)('accepts late %s stop evidence without changing the settled unknown outcome', async evidence => {
    const f = await fixture(), release = deferred<void>(), started = deferred<void>(), abort = new AbortController();
    cleanups.push(async () => { release.resolve(); });
    helperStream(f, async function* () {
      yield { type: 'init', text: 'owned-command' }; started.resolve();
      await release.promise;
      if (evidence === 'terminal') yield { type: 'execution_complete' };
    });
    f.commands.interrupt.mockResolvedValue(undefined);
    f.commands.getCommandStatus.mockResolvedValue({ id: 'owned-command', running: true, exitCode: null });
    const writing = f.fs.writeText(f.target, 'secret', undefined, abort.signal).catch(error => error);
    await started.promise; abort.abort(); const original = await writing;
    expect(original).toMatchObject({ outcome: 'unknown', remoteState: 'unknown' });
    const disposing = f.fs.dispose();
    if (evidence === 'status') f.commands.getCommandStatus.mockResolvedValue({ id: 'owned-command', running: false, exitCode: 0 });
    release.resolve(); await expect(disposing).resolves.toBeUndefined();
    expect(await writing).toBe(original); expect(original.remoteState).toBe('unknown');
    expect(f.commands.interrupt).toHaveBeenCalledExactlyOnceWith('owned-command');
    expect(await f.artifactRoots()).not.toEqual([]);
  });

  it('observes and interrupts an init arriving after unknown settlement, during disposal', async () => {
    const f = await fixture(), release = deferred<void>(), started = deferred<void>(), abort = new AbortController();
    cleanups.push(async () => { release.resolve(); });
    helperStream(f, async function* () {
      started.resolve(); await release.promise; yield { type: 'init', text: 'late-command' };
    });
    f.commands.interrupt.mockResolvedValue(undefined);
    f.commands.getCommandStatus.mockResolvedValue({ id: 'late-command', running: false, exitCode: 0 });
    const writing = f.fs.writeText(f.target, 'secret', undefined, abort.signal).catch(error => error);
    await started.promise; abort.abort();
    expect(await writing).toMatchObject({ outcome: 'unknown', commandId: undefined });
    const disposing = f.fs.dispose(); release.resolve(); await expect(disposing).resolves.toBeUndefined();
    expect(f.commands.interrupt).toHaveBeenCalledExactlyOnceWith('late-command');
  });

  it('retains stop ownership and interrupts an init even after disposal has rejected', async () => {
    const f = await fixture(), release = deferred<void>(), started = deferred<void>(), abort = new AbortController();
    cleanups.push(async () => { release.resolve(); });
    helperStream(f, async function* () {
      started.resolve(); await release.promise; yield { type: 'init', text: 'late-command' };
      yield { type: 'execution_complete' };
    });
    const writing = f.fs.writeText(f.target, 'secret', undefined, abort.signal).catch(error => error);
    await started.promise; abort.abort(); await writing;
    await expect(f.fs.dispose()).rejects.toMatchObject({ outcome: 'unknown', commandId: undefined });
    release.resolve(); await expect.poll(() => f.commands.interrupt.mock.calls.length).toBe(1);
    expect(f.commands.interrupt).toHaveBeenCalledExactlyOnceWith('late-command');
    await expect(f.fs.dispose()).rejects.toMatchObject({ outcome: 'unknown', commandId: undefined });
  });

  it('joins every owned command before rejecting disposal and caches that failure', async () => {
    const f = await fixture(); let serial = 0;
    helperStream(f, async function* () { yield { type: 'init', text: `owned-${++serial}` }; });
    f.commands.getCommandStatus.mockResolvedValue({ running: true, exitCode: null });
    await Promise.all([f.fs.writeText(f.target, 'one'), f.fs.writeText(f.target, 'two')].map(work => expect(work).rejects.toMatchObject({ outcome: 'unknown' })));
    const stopped = new Set<string>();
    f.commands.interrupt.mockImplementation(async id => { if (id === 'owned-2') stopped.add(id); });
    f.commands.getCommandStatus.mockImplementation(async id => ({ id, running: !stopped.has(id), exitCode: stopped.has(id) ? 0 : null }));
    const error = await f.fs.dispose().catch(error => error);
    expect(error).toMatchObject({ outcome: 'unknown', commandId: 'owned-1' });
    expect(f.commands.interrupt.mock.calls.map(call => call[0]).sort()).toEqual(['owned-1', 'owned-2']);
    expect(await f.fs.dispose().catch(error => error)).toBe(error);
    expect(serial).toBe(2);
  });

  it.each(['closed', 'killed'] as const)('treats a %s binding conservatively', async state => {
    const f = await fixture(); helperStream(f, async function* () {});
    await expect(f.fs.writeText(f.target, 'secret')).rejects.toMatchObject({ outcome: 'unknown' });
    if (state === 'killed') { await f.binding.kill(); await expect(f.fs.dispose()).resolves.toBeUndefined(); }
    else { await f.binding.close(); await expect(f.fs.dispose()).rejects.toMatchObject({ outcome: 'unknown' }); }
    expect(f.commands.interrupt).not.toHaveBeenCalled(); expect(f.commands.getCommandStatus).not.toHaveBeenCalled();
  });

  it.each(['execution_complete', 'numeric exit', 'generic error'] as const)('requires trusted terminal evidence for %s', async terminal => {
    const f = await fixture();
    helperStream(f, async function* () {
      yield { type: 'init', text: 'owned-command' };
      if (terminal === 'execution_complete') yield { type: 'execution_complete' };
      else yield { type: 'error', error: { ename: terminal === 'numeric exit' ? 'CommandExecError' : 'TransportError', evalue: '1' } };
    });
    f.commands.getCommandStatus.mockResolvedValue({ id: 'owned-command', running: true, exitCode: null });
    await expect(f.fs.writeText(f.target, 'secret')).rejects.toMatchObject({ outcome: 'unknown', remoteState: terminal === 'generic error' ? 'unknown' : 'stopped' });
    if (terminal === 'generic error') await expect(f.fs.dispose()).rejects.toMatchObject({ outcome: 'unknown', remoteState: 'unknown' });
    else await expect(f.fs.dispose()).resolves.toBeUndefined();
  });

  it('does not confuse semantic errors, stopped response loss, or upload uncertainty with command termination', async () => {
    const semantic = await fixture();
    await expect(semantic.fs.readText(await semantic.fs.resolve('missing'))).rejects.toMatchObject({ code: 'FS_NOT_FOUND', remoteState: 'stopped' });
    await expect(semantic.fs.dispose()).resolves.toBeUndefined();
    const stopped = await fixture(); stopped.controls.transformResponse = () => Buffer.from('{}');
    await expect(stopped.fs.writeText(stopped.target, 'committed')).rejects.toMatchObject({ outcome: 'unknown', remoteState: 'stopped' });
    await expect(stopped.fs.dispose()).resolves.toBeUndefined(); expect(await stopped.artifactRoots()).not.toEqual([]);
    const upload = await fixture(), count = upload.requestCount();
    upload.files.writeFiles.mockRejectedValue(new Error('private upload error'));
    await expect(upload.fs.writeText(upload.target, 'not launched')).rejects.toMatchObject({ outcome: 'unknown' });
    await expect(upload.fs.dispose()).resolves.toBeUndefined(); expect(upload.requestCount()).toBe(count);
    expect(await upload.artifactRoots()).not.toEqual([]);
  });
});
