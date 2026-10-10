// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { chmod, mkdir, readFile, stat, symlink, unlink, writeFile } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { Context } from '@deepseek-ai/cordis';
import { FileSystem, FsError, FsTargetKey, FsVersion } from '@deepseek-ai/dsh-fs';
import { afterEach, describe, expect, it } from 'vitest';
import { RemoteFileSystem } from '../src/filesystem.js';
import { HelperClient, MAX_DATA_BYTES, typedFsError } from '../src/helper-client.js';
import { helperSdk } from './fixtures/helper-sdk.js';
import { deferred } from './fixtures/sdk-facade.js';

const cleanups: (() => Promise<void>)[] = [];
async function fixture(id?: string, session?: string, options = {}) {
  const sdk = await helperSdk(id, session); const ctx = new Context();
  const fs = new RemoteFileSystem(ctx, { binding: sdk.binding, timeoutMs: 2_000, settlementTimeoutMs: 100, ...options });
  cleanups.push(async () => { await fs.dispose().catch(() => undefined); await sdk.cleanup(); });
  return { ...sdk, ctx, fs };
}
async function streamed(fs: RemoteFileSystem, target: Parameters<FileSystem['readText']>[0], signal?: AbortSignal) {
  let result = ''; for await (const chunk of await fs.streamText(target, signal)) result += chunk; return result;
}
afterEach(async () => { for (const cleanup of cleanups.splice(0)) await cleanup(); });

describe('remote dsh filesystem using actual helper protocol', () => {
  it('exposes eager verified readiness for composition before Agent creation', async () => {
    const f = await fixture(); await f.fs.ready();
    expect(f.uploads.some(item => item.path.endsWith('/fs_helper.py'))).toBe(true);
    expect(f.uploads.filter(item => item.path.endsWith('/request.json')).map(item => JSON.parse(Buffer.from(item.data).toString()).operation)).toEqual(['resolve', 'stat']);
  });
  it('implements installed Fs SPI through a real Cordis service proxy', async () => {
    const f = await fixture();
    expect(f.fs).toBeInstanceOf(FileSystem);
    const target = await f.ctx.fs.resolve('a');
    expect(f.ctx.fs.processPath(target)).toBe('/workspace/a');
    await f.ctx.fs.writeText(target, 'hello'); expect(await f.ctx.fs.readText(target)).toBe('hello');
    expect(f.ctx.fs.processPathFromHostPath('/etc/passwd')).toBeUndefined();
    await expect(f.ctx.fs.watch(target, () => undefined, new AbortController().signal)).rejects.toBeInstanceOf(FsError);
    expect(f.ctx.fs.sandboxMode).toBeUndefined();
  });
  it('qualifies target keys by sandbox and session and rejects foreign or fabricated targets', async () => {
    const a = await fixture('one', 's1'), b = await fixture('two', 's1'), c = await fixture('one', 's2');
    const ta = await a.fs.resolve('same'), tb = await b.fs.resolve('same'), tc = await c.fs.resolve('same');
    expect(new Set([ta.targetKey, tb.targetKey, tc.targetKey]).size).toBe(3);
    expect(Object.isFrozen(ta)).toBe(true);
    await a.fs.writeText(ta, 'a'); await b.fs.writeText(tb, 'b');
    expect(await a.fs.readText(ta)).toBe('a'); expect(await b.fs.readText(tb)).toBe('b');
    for (const bad of [tb, { ...ta }, { targetKey: FsTargetKey('/etc/passwd'), displayPath: '/etc/passwd' }]) {
      expect(() => a.fs.processPath(bad)).toThrow(FsError);
      await expect(a.fs.writeText(bad, 'unsafe')).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    }
  });
  it('uses remote cwd, physical symlink traversal and stable saved alias identity', async () => {
    const f = await fixture(); await mkdir(f.local('/workspace/real/sub'), { recursive: true });
    await symlink('real/sub', f.local('/workspace/link'));
    const target = await f.fs.resolve('link/../a #?%');
    expect(f.fs.processPath(target)).toBe('/workspace/real/a #?%');
    expect(f.fs.fileUrl(target)).toBe('file:///workspace/real/a%20%23%3F%25');
    expect(f.fs.contains(await f.fs.resolve('real'), target)).toBe(true);
    expect(f.fs.contains(await f.fs.resolve('real2'), target)).toBe(false);
    const viaCwd = await f.fs.resolve('../a #?%', { cwd: '/workspace/link' });
    expect(viaCwd.targetKey).toBe(target.targetKey);
    await unlink(f.local('/workspace/link')); await symlink('other', f.local('/workspace/link'));
    await f.fs.writeText(target, 'saved'); expect(await readFile(f.local('/workspace/real/a #?%'), 'utf8')).toBe('saved');
  });
  it('canonicalizes missing suffixes and dangling links without replacing the alias', async () => {
    const f = await fixture(); await symlink('missing/parent/file', f.local('/workspace/dangling'));
    const target = await f.fs.resolve('dangling'); expect(f.fs.processPath(target)).toBe('/workspace/missing/parent/file');
    await f.fs.writeText(target, 'new', { kind: 'createIfAbsent' });
    expect((await f.fs.lstat('dangling'))?.type).toBe('symlink'); expect(await f.fs.readText(target)).toBe('new');
  });
  it('returns content-free stable listings and stat/lstat metadata', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/z'), 'z'); await writeFile(f.local('/workspace/a'), 'abc');
    await symlink('a', f.local('/workspace/link'));
    expect(await f.fs.stat(await f.fs.resolve('absent'))).toBeUndefined();
    expect(await f.fs.lstat('absent')).toBeUndefined(); expect((await f.fs.lstat('link'))?.type).toBe('symlink');
    const entries = await f.fs.listDir(await f.fs.resolve('.'));
    expect(entries.map(item => item.name)).toEqual(['a', 'link', 'z']);
    expect(entries[0]).toMatchObject({ type: 'file', size: 3 });
    expect(entries[1]!.target.targetKey).toBe(entries[0]!.target.targetKey);
    expect(f.uploads.filter(item => item.path.endsWith('/request.json')).map(item => JSON.parse(Buffer.from(item.data).toString()).operation)).not.toContain('read-text');
    await expect(f.fs.listDir(await f.fs.resolve('a'))).rejects.toMatchObject({ code: 'FS_NOT_DIRECTORY' });
  });
  it.each([[[0xff, 0xfe]], [[97, 0, 98]], [[0xe2, 0x82]]])('rejects non-text bytes %j on whole and stream reads', async bytes => {
    const f = await fixture(); await writeFile(f.local('/workspace/binary'), Buffer.from(bytes)); const target = await f.fs.resolve('binary');
    await expect(f.fs.readText(target)).rejects.toMatchObject({ code: 'FS_NOT_TEXT' });
    await expect(streamed(f.fs, target)).rejects.toMatchObject({ code: 'FS_NOT_TEXT' });
    expect(await f.fs.readBytes(target, undefined, bytes.length)).toEqual(Uint8Array.from(bytes));
  });
  it('preserves BOM semantics, UTF-8 boundaries, and unmodified newlines', async () => {
    const f = await fixture(); const content = '\ufeff' + 'a'.repeat(65_531) + '世😀\r\nend';
    await writeFile(f.local('/workspace/text'), content); const target = await f.fs.resolve('text');
    expect(await streamed(f.fs, target)).toBe(content.slice(1)); expect(await f.fs.readText(target)).toBe(content.slice(1));
  });
  it('rejects changes between stream chunks without splicing file versions', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/text'), 'a'.repeat(130_000)); const target = await f.fs.resolve('text');
    const iterator = (await f.fs.streamText(target))[Symbol.asyncIterator]();
    expect((await iterator.next()).value).toBe('a'.repeat(65_536));
    await writeFile(f.local('/workspace/text'), 'b'.repeat(130_000));
    await expect(iterator.next()).rejects.toMatchObject({ code: 'FS_STALE_VERSION' });
  });
  it('bounds complete byte reads and each byte window without truncation as success', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/bytes'), Buffer.from([0, 255, 128, 4])); const target = await f.fs.resolve('bytes');
    expect(await f.fs.readBytes(target, undefined, 4)).toEqual(Uint8Array.from([0, 255, 128, 4]));
    await expect(f.fs.readBytes(target, undefined, 3)).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
    expect(await f.fs.readByteRange(target, { offset: 1, length: 2 })).toEqual(Uint8Array.from([255, 128]));
    expect(await f.fs.readByteRange(target, { offset: 99, length: 2 })).toEqual(new Uint8Array());
    expect(await f.fs.readByteRange(target, { offset: 0, length: 0 })).toEqual(new Uint8Array());
    for (const range of [{ offset: -1, length: 1 }, { offset: 0, length: 1.5 }, { offset: Number.MAX_SAFE_INTEGER, length: 1 }]) {
      await expect(f.fs.readByteRange(target, range)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    }
    await expect(f.fs.readBytes(target, undefined, MAX_DATA_BYTES + 1)).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
    await expect(f.fs.readByteRange(target, { offset: 0, length: MAX_DATA_BYTES + 1 })).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
  });
  it('enforces inclusive text caps and bounded stream total', async () => {
    const f = await fixture(); const target = await f.fs.resolve('large');
    await f.fs.writeText(target, 'a'.repeat(MAX_DATA_BYTES));
    expect((await f.fs.readText(target)).length).toBe(MAX_DATA_BYTES);
    expect((await streamed(f.fs, target)).length).toBe(MAX_DATA_BYTES);
    await writeFile(f.local('/workspace/large'), 'a'.repeat(MAX_DATA_BYTES + 1));
    await expect(streamed(f.fs, target)).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
    await expect(f.fs.writeText(target, '世'.repeat(Math.ceil(MAX_DATA_BYTES / 3)))).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
  });
  it('forwards atomic write/edit intent and returns exact diff/newline/version semantics', async () => {
    const f = await fixture(); const target = await f.fs.resolve('new');
    const created = await f.fs.writeText(target, 'a\r\nb\r\n', { kind: 'createIfAbsent' });
    expect(created).toMatchObject({ operation: 'create', before: null, after: 'a\nb\n' });
    const edited = await f.fs.editText(target, { oldString: 'a', newString: 'x\r', replaceAll: false }, { version: created.version });
    expect(edited.before).toBe('a\nb\n'); expect(await f.fs.readText(target)).toBe('x\r\nb\r\n');
    expect((await f.fs.stat(target))?.version).toBe(edited.version);
    await expect(f.fs.writeText(target, 'bad', { kind: 'createIfAbsent' })).rejects.toMatchObject({ code: 'FS_NOT_OBSERVED' });
    await expect(f.fs.writeText(target, 'bad', { kind: 'replaceIfVersion', version: created.version })).rejects.toMatchObject({ code: 'FS_STALE_VERSION' });
    await expect(f.fs.editText(target, { oldString: 'no', newString: '', replaceAll: false }, { version: created.version })).rejects.toMatchObject({ code: 'FS_STALE_VERSION' });
    await expect(f.fs.editText(target, { oldString: 'no', newString: '', replaceAll: false })).rejects.toMatchObject({ code: 'FS_EDIT_NOT_FOUND' });
    await f.fs.writeText(target, 'a a');
    await expect(f.fs.editText(target, { oldString: 'a', newString: 'b', replaceAll: false })).rejects.toMatchObject({ code: 'FS_AMBIGUOUS_EDIT' });
    await f.fs.editText(target, { oldString: 'a', newString: 'b', replaceAll: true }); expect(await f.fs.readText(target)).toBe('b b');
  });
  it('serializes actual helper writers and stale-checks canonical aliases', async () => {
    const f = await fixture(); const target = await f.fs.resolve('race'); const initial = await f.fs.writeText(target, 'a');
    await symlink('race', f.local('/workspace/alias')); const alias = await f.fs.resolve('alias');
    const results = await Promise.allSettled([f.fs.writeText(target, 'b', { kind: 'replaceIfVersion', version: initial.version }),
      f.fs.editText(alias, { oldString: 'a', newString: 'c', replaceAll: false }, { version: initial.version })]);
    expect(results.filter(result => result.status === 'fulfilled')).toHaveLength(1);
    expect(results.find(result => result.status === 'rejected')).toMatchObject({ reason: { code: 'FS_STALE_VERSION' } });
  });
  it('validates text, expected intent and unsupported policy before remote mutation', async () => {
    const f = await fixture(); const target = await f.fs.resolve('safe'); const count = f.requestCount();
    await expect(f.fs.writeText(target, '\ud800')).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    await expect(f.fs.writeText(target, 'a\0b')).rejects.toMatchObject({ code: 'FS_NOT_TEXT' });
    await expect(f.fs.writeText(target, 'a', { kind: 'replaceIfVersion', version: FsVersion('v'), surprise: true } as never)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    await expect(f.fs.editText(target, { oldString: '', newString: '', replaceAll: false })).rejects.toMatchObject({ code: 'FS_EDIT_NOT_FOUND' });
    await expect(f.fs.writeText(target, 'a', undefined, undefined, { mode: 'read-only' } as never)).rejects.toMatchObject({ code: 'FS_SANDBOX_DENIED' });
    await expect(f.fs.writeText(target, 'a', undefined, undefined, { mode: 'danger-full-access', sessionId: 'foreign' } as never)).rejects.toMatchObject({ code: 'FS_SANDBOX_DENIED' });
    expect(f.requestCount()).toBe(count);
  });
  it('pre-abort performs no setup or launch; a stream abort between chunks is typed', async () => {
    const f = await fixture(); const abort = new AbortController(); abort.abort();
    await expect(f.fs.resolve('none', { signal: abort.signal })).rejects.toMatchObject({ code: 'FS_ABORTED' });
    expect(f.calls).toHaveLength(0); expect(f.uploads).toHaveLength(0);
    await writeFile(f.local('/workspace/text'), 'a'.repeat(130_000)); const target = await f.fs.resolve('text');
    const running = new AbortController(); const iterator = (await f.fs.streamText(target, running.signal))[Symbol.asyncIterator]();
    await iterator.next(); running.abort(); await expect(iterator.next()).rejects.toMatchObject({ code: 'FS_ABORTED' });
  });
  it('installs only the static asset in private directories and checks hash plus actual protocol', async () => {
    const f = await fixture(); await f.fs.resolve('a');
    const helper = f.uploads.find(item => item.path.endsWith('/fs_helper.py'))!;
    expect(Buffer.from(helper.data)).toEqual(await readFile(new URL('../remote/fs_helper.py', import.meta.url)));
    expect(helper.mode).toBe(600);
    expect(f.calls.every(call => Array.isArray(call.argv) && (call.options as { workingDirectory: string }).workingDirectory === '/')).toBe(true);
    expect(f.calls.some(call => call.argv.includes('a'))).toBe(false);
    expect(f.uploads.filter(item => item.path.endsWith('/request.json')).every(item => item.mode === 600)).toBe(true);
  });
  it('rejects corrupted helper readiness without launching any target mutation', async () => {
    const f = await fixture(); const original = f.files.writeFiles.getMockImplementation()!;
    f.files.writeFiles.mockImplementation(entries => original(entries.map(entry => entry.path.endsWith('/fs_helper.py') ? { ...entry, data: Buffer.from('wrong helper') } : entry)));
    await expect(f.fs.resolve('a')).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(f.requestCount()).toBe(0);
  });
  it.each(['0', '1', '2'] as const)('verifies helper readiness with PYTHONOPTIMIZE=%s and rejects valid changed code before protocol launch', async optimize => {
    const good = await fixture(); good.controls.pythonOptimize = optimize;
    await good.fs.ready(); expect(good.requestCount()).toBe(2);
    const changed = await fixture(); changed.controls.pythonOptimize = optimize;
    const original = changed.files.writeFiles.getMockImplementation()!;
    changed.files.writeFiles.mockImplementation(entries => original(entries.map(entry => entry.path.endsWith('/fs_helper.py')
      ? { ...entry, data: Buffer.from(Buffer.from(entry.data as Uint8Array).toString('utf8').replace('Copyright', 'copyright')) } : entry)));
    await expect(changed.fs.ready()).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(changed.requestCount()).toBe(0);
  });
  it('cannot use closed bindings or a disposed composition', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a'); await f.binding.close();
    await expect(f.fs.readText(target)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    await f.fs.dispose(); await expect(f.fs.resolve('a')).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
  });
  it('maps all FsError codes without retaining arbitrary remote diagnostics', () => {
    for (const code of ['FS_NOT_FOUND', 'FS_NOT_DIRECTORY', 'FS_NOT_TEXT', 'FS_NOT_REGULAR_FILE', 'FS_TOO_LARGE', 'FS_PERMISSION_DENIED', 'FS_SANDBOX_DENIED', 'FS_IO_ERROR', 'FS_STALE_VERSION', 'FS_NOT_OBSERVED', 'FS_AMBIGUOUS_EDIT', 'FS_EDIT_NOT_FOUND', 'FS_ABORTED'] as const) {
      expect(typedFsError(code)).toBeInstanceOf(FsError); expect(typedFsError(code).code).toBe(code);
    }
  });
  it('rejects invalid helper args before setup and never reads a caller-selected host asset', async () => {
    const f = await fixture(); const client = new HelperClient(f.binding);
    await expect(client.request('resolve', { path: 'a', cwd: 'relative' })).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    await expect(client.request('resolve', { path: '\ud800', cwd: '/workspace' })).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    await expect(client.request('resolve', { path: 'a', cwd: '/workspace', script: '/etc/passwd' } as never)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(f.calls).toHaveLength(0); await client.dispose();
  });
  it.each([
    '{"protocol":2,"ok":true,"result":{"version":"v","text":"private"}}',
    '{"protocol":1,"protocol":1,"ok":true,"result":{"version":"v","text":"private"}}',
    '{"protocol":1,"ok":true,"result":{"version":"v","text":"private","extra":1}}',
    '{"protocol":1,"ok":true,"result":{"version":"v","text":"\\ud800"}}',
    '{"protocol":1,"ok":true,"result":',
  ])('fails closed for malformed completed responses', async wire => {
    const f = await fixture(); const target = await f.fs.resolve('a');
    f.controls.transformResponse = () => Buffer.from(wire);
    const error = await f.fs.readText(target).catch(error => error);
    expect(error).toBeInstanceOf(FsError); expect(error).toMatchObject({ code: 'FS_IO_ERROR' });
    expect(error.message).not.toContain('private'); expect(error.cause).toBeUndefined();
  });
  it('decodes a valid typed error but ignores untrusted remote message text', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a');
    f.controls.transformResponse = () => Buffer.from(JSON.stringify({ protocol: 1, ok: false, error: { code: 'FS_PERMISSION_DENIED', message: 'private token and URL' } }));
    await expect(f.fs.readText(target)).rejects.toMatchObject({ code: 'FS_PERMISSION_DENIED' });
    expect((await f.fs.readText(target).catch(error => error)).message).not.toContain('private');
  });
  it('bounds encoded download and rejects overlong raw responses', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a');
    f.controls.transformResponse = () => new Uint8Array(16 * 1024 * 1024 + 1);
    await expect(f.fs.readText(target)).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
  });
  it('cancels before helper launch even after request upload finishes', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a'); const abort = new AbortController();
    const original = f.files.writeFiles.getMockImplementation()!;
    f.files.writeFiles.mockImplementation(async entries => { await original(entries); if (entries[0]?.path.endsWith('/request.json')) abort.abort(); });
    const count = f.requestCount(); await expect(f.fs.writeText(target, 'a', undefined, abort.signal)).rejects.toMatchObject({ code: 'FS_ABORTED' });
    expect(f.requestCount()).toBe(count); await expect(readFile(f.local('/workspace/a'))).rejects.toMatchObject({ code: 'ENOENT' });
  });
  it('explicitly interrupts a known helper despite unreliable SSE abort, observing a precommit typed cancellation', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a'); await f.fs.writeText(target, 'original');
    const lockPath = f.local('/workspace/.opensandbox-dsh-locks-v1/') + createHash('sha256').update(f.local('/workspace/a')).digest('hex') + '.lock';
    const holder = spawn('python3', ['-c', 'import fcntl,sys,time; f=open(sys.argv[1],"rb"); fcntl.flock(f,fcntl.LOCK_EX); print("locked",flush=True); time.sleep(10)', lockPath]);
    await new Promise<void>(resolve => holder.stdout.once('data', () => resolve()));
    const abort = new AbortController();
    f.controls.afterLaunch = async argv => {
      if (!argv[1]?.endsWith('/fs_helper.py')) return;
      for (;;) { try { await stat(f.local(argv[3]!)); break; } catch { await new Promise(resolve => setTimeout(resolve, 5)); } }
      abort.abort();
    };
    try {
      await expect(f.fs.writeText(target, 'never', undefined, abort.signal)).rejects.toMatchObject({ code: 'FS_ABORTED' });
      expect(await readFile(f.local('/workspace/a'), 'utf8')).toBe('original');
      expect(f.commands.interrupt).toHaveBeenCalledTimes(1);
    } finally { holder.kill('SIGKILL'); }
  });
  it('preserves known committed success when cancellation races the terminal event', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a'); const abort = new AbortController();
    f.controls.afterExit = argv => { if (argv[1]?.endsWith('/fs_helper.py')) abort.abort(); };
    const result = await f.fs.writeText(target, 'committed', undefined, abort.signal);
    expect(result.after).toBe('committed'); expect(await readFile(f.local('/workspace/a'), 'utf8')).toBe('committed');
  });
  it('reads known completed response after missing terminal SSE using bounded command status', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a');
    f.controls.dropTerminal = true;
    expect((await f.fs.writeText(target, 'committed')).after).toBe('committed');
    expect(f.commands.getCommandStatus).toHaveBeenCalled();
  });
  it('retains artifacts and never retries when a mutation commits but launch observation is lost', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a'); const count = f.requestCount();
    f.controls.dropInit = true; f.controls.dropTerminal = true;
    await expect(f.fs.writeText(target, 'committed')).rejects.toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown' });
    expect(await readFile(f.local('/workspace/a'), 'utf8')).toBe('committed'); expect(f.requestCount()).toBe(count + 1);
    const deleted = f.files.deleteDirectories.mock.calls.length; await f.fs.dispose();
    expect(f.files.deleteDirectories.mock.calls.length).toBe(deleted); expect(await f.artifactRoots()).not.toHaveLength(0);
  });
  it('retains unknown mutation artifacts for malformed or missing postcommit response', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a'); const count = f.requestCount();
    f.controls.transformResponse = () => Buffer.from('{}');
    await expect(f.fs.writeText(target, 'committed')).rejects.toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown' });
    expect(await readFile(f.local('/workspace/a'), 'utf8')).toBe('committed'); expect(f.requestCount()).toBe(count + 1);
  });
  it('bounds a hung postheaders stream and hung interrupt/status without claiming termination', async () => {
    const f = await fixture(undefined, undefined, { timeoutMs: 100, settlementTimeoutMs: 40 }); const target = await f.fs.resolve('a');
    f.controls.dropTerminal = true; f.controls.hangAfterExit = true;
    f.commands.interrupt.mockImplementation(() => new Promise(() => undefined));
    f.commands.getCommandStatus.mockImplementation(() => new Promise(() => undefined));
    await expect(f.fs.writeText(target, 'committed')).rejects.toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown', remoteState: 'unknown' });
    expect(f.commands.interrupt).toHaveBeenCalledTimes(1);
  });
  it('rejects accessor and hidden toJSON request objects before remote effects', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a'); const count = f.requestCount();
    const expected = { kind: 'createIfAbsent' };
    Object.defineProperty(expected, 'toJSON', { value: () => undefined });
    await expect(f.fs.writeText(target, 'a', expected as never)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    const range = { offset: 0, get length() { return 1; } };
    await expect(f.fs.readByteRange(target, range)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(f.requestCount()).toBe(count);
  });
  it('snapshots byte ranges before awaiting SDK I/O', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/a'), 'abc'); const target = await f.fs.resolve('a');
    const range = { offset: 0, length: 2 }; const original = f.files.writeFiles.getMockImplementation()!;
    f.files.writeFiles.mockImplementation(async entries => { await original(entries); if (entries[0]?.path.endsWith('/request.json')) range.length = 0; });
    expect(await f.fs.readByteRange(target, range)).toEqual(Uint8Array.from([97, 98]));
  });
  it('bounds and decodes an inclusive one-MiB binary result', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/a'), Buffer.alloc(MAX_DATA_BYTES, 255)); const target = await f.fs.resolve('a');
    const bytes = await f.fs.readBytes(target, undefined, MAX_DATA_BYTES); expect(bytes.byteLength).toBe(MAX_DATA_BYTES); expect(bytes[0]).toBe(255);
  });
  it('aborts a cancelled waiter on shared helper readiness before any target send', async () => {
    const f = await fixture(); const client = new HelperClient(f.binding, { timeoutMs: 2_000 }); cleanups.push(() => client.dispose());
    const entered = deferred<void>(), release = deferred<void>(); const original = f.files.writeFiles.getMockImplementation()!;
    f.files.writeFiles.mockImplementation(async entries => { if (entries[0]?.path.endsWith('/fs_helper.py')) { entered.resolve(); await release.promise; } await original(entries); });
    const preparing = client.ready(); await entered.promise; const abort = new AbortController();
    const request = client.request('resolve', { path: 'a', cwd: '/workspace' }, abort.signal); abort.abort();
    const result = await Promise.race([request.then(() => 'success', error => error), new Promise(resolve => setTimeout(() => resolve('not cancelled'), 100))]);
    release.resolve(); await preparing;
    expect(result).toMatchObject({ code: 'FS_ABORTED' });
    await expect(request).rejects.toMatchObject({ code: 'FS_ABORTED' });
  });
  it('checks path and maximally escaped JSON request limits before transport', async () => {
    const f = await fixture();
    await expect(f.fs.resolve('x'.repeat(4097))).rejects.toMatchObject({ code: 'FS_TOO_LARGE' }); expect(f.calls).toHaveLength(0);
    const target = await f.fs.resolve('a'); const count = f.requestCount();
    await expect(f.fs.editText(target, { oldString: '\x01'.repeat(MAX_DATA_BYTES), newString: '\x02'.repeat(MAX_DATA_BYTES), replaceAll: true })).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
    expect(f.requestCount()).toBe(count);
  });
  it('reads bounded windows from files larger than the complete-read cap', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/a'), Buffer.alloc(MAX_DATA_BYTES + 100, 255)); const target = await f.fs.resolve('a');
    expect(await f.fs.readByteRange(target, { offset: MAX_DATA_BYTES + 90, length: 100 })).toEqual(new Uint8Array(10).fill(255));
    await expect(f.fs.readBytes(target, undefined, MAX_DATA_BYTES)).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
  });
  it('keeps POSIX permission bits and exposes helper lock metadata in listings', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/a'), 'a'); await chmod(f.local('/workspace/a'), 0o640);
    const target = await f.fs.resolve('a'); await f.fs.writeText(target, 'b');
    expect((await stat(f.local('/workspace/a'))).mode & 0o777).toBe(0o640);
    const entries = await f.fs.listDir(await f.fs.resolve('.')); expect(entries.map(entry => entry.name)).toContain('.opensandbox-dsh-locks-v1');
    const helper = f.uploads.find(item => item.path.endsWith('/fs_helper.py'))!;
    expect((await stat(f.local(helper.path.slice(0, helper.path.lastIndexOf('/'))))).mode & 0o777).toBe(0o700);
  });
  it('uses deterministic codepoint ordering and rejects oversized direct listings', async () => {
    const f = await fixture(); await mkdir(f.local('/workspace/list'));
    const names = ['\ue000', '\u{10000}', ...Array.from({ length: 4094 }, (_, index) => String(index).padStart(4, '0'))];
    await Promise.all(names.map(name => writeFile(f.local('/workspace/list/' + name), '')));
    const target = await f.fs.resolve('list'); const entries = await f.fs.listDir(target);
    expect(entries).toHaveLength(4096); expect(entries.slice(-2).map(entry => entry.name)).toEqual(['\ue000', '\u{10000}']);
    await writeFile(f.local('/workspace/list/overflow'), '');
    await expect(f.fs.listDir(target)).rejects.toMatchObject({ code: 'FS_TOO_LARGE' });
  });
  it('rejects invalid and overlong base64 windows without silently decoding them', async () => {
    const f = await fixture(); const target = await f.fs.resolve('a');
    for (const base64 of ['%%%=', 'YR==', 'YWJj']) {
      f.controls.transformResponse = () => Buffer.from(JSON.stringify({ protocol: 1, ok: true, result: { version: 'v', base64 } }));
      await expect(f.fs.readByteRange(target, { offset: 0, length: 1 })).rejects.toBeInstanceOf(FsError);
    }
  });
  it('retains unknown upload preparation and never launches a target helper afterwards', async () => {
    const f = await fixture(undefined, undefined, { timeoutMs: 100, settlementTimeoutMs: 40 }); const target = await f.fs.resolve('a');
    const count = f.requestCount(), deleted = f.files.deleteDirectories.mock.calls.length;
    const original = f.files.writeFiles.getMockImplementation()!;
    f.files.writeFiles.mockImplementation(async entries => { if (entries[0]?.path.endsWith('/request.json')) await new Promise(() => undefined); await original(entries); });
    await expect(f.fs.writeText(target, 'a')).rejects.toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown' });
    expect(f.requestCount()).toBe(count); expect(f.files.deleteDirectories.mock.calls.length).toBe(deleted);
  });
  it('observes late init after bounded unknown settlement and explicitly interrupts the late ID', async () => {
    const f = await fixture(undefined, undefined, { timeoutMs: 100, settlementTimeoutMs: 40 }); const target = await f.fs.resolve('a');
    const original = f.commands.runStream.getMockImplementation()!;
    f.commands.runStream.mockImplementation(async function* (argv, options, signal) {
      for await (const event of original(argv, options, signal)) {
        if (Array.isArray(argv) && argv[1]?.endsWith('/fs_helper.py') && event.type === 'init') await new Promise(resolve => setTimeout(resolve, 180));
        yield event;
      }
    });
    await expect(f.fs.writeText(target, 'a')).rejects.toMatchObject({ outcome: 'unknown' });
    await new Promise(resolve => setTimeout(resolve, 100)); expect(f.commands.interrupt).toHaveBeenCalledTimes(1);
  });
  it('tears down the actual Cordis plugin and cleans only known completed helper artifacts', async () => {
    const f = await fixture(); const ctx = new Context(); const fiber = ctx.plugin(RemoteFileSystem, { binding: f.binding }); await fiber;
    const target = await ctx.fs.resolve('a'); await ctx.fs.writeText(target, 'a');
    await fiber.dispose(); expect(ctx.get('fs')).toBeUndefined(); expect(await f.artifactRoots()).toHaveLength(0);
    expect(await readFile(f.local('/workspace/a'), 'utf8')).toBe('a');
  });
  it('handles empty files at a zero-byte cap and consistently rejects directories as file content', async () => {
    const f = await fixture(); await writeFile(f.local('/workspace/empty'), ''); const empty = await f.fs.resolve('empty');
    expect(await f.fs.readBytes(empty, undefined, 0)).toEqual(new Uint8Array()); expect(await streamed(f.fs, empty)).toBe('');
    const directory = await f.fs.resolve('.');
    await expect(f.fs.readText(directory)).rejects.toMatchObject({ code: 'FS_NOT_REGULAR_FILE' });
    await expect(f.fs.readBytes(directory, undefined, MAX_DATA_BYTES)).rejects.toMatchObject({ code: 'FS_NOT_REGULAR_FILE' });
    await expect(streamed(f.fs, directory)).rejects.toMatchObject({ code: 'FS_NOT_REGULAR_FILE' });
  });
  it('rejects a non-enumerable own create guard without overwriting the existing file or sending any mutation', async () => {
    const f = await fixture(); const client = new HelperClient(f.binding); cleanups.unshift(() => client.dispose());
    const target = { path: '/workspace/a', displayPath: '/workspace/a' };
    await client.request('write-text', { target, content: 'original' }); const count = f.requestCount();
    const args = { target, content: 'overwritten' }; Object.defineProperty(args, 'expected', { value: { kind: 'createIfAbsent' } });
    await expect(client.request('write-text', args)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
    expect(await readFile(f.local('/workspace/a'), 'utf8')).toBe('original'); expect(f.requestCount()).toBe(count);
  });
  it('isolates initiating readiness cancellation from live waiters and later requests', async () => {
    const f = await fixture(); const entered = deferred<void>(), release = deferred<void>();
    const original = f.files.writeFiles.getMockImplementation()!;
    f.files.writeFiles.mockImplementation(async entries => { if (entries[0]?.path.endsWith('/fs_helper.py')) { entered.resolve(); await release.promise; } await original(entries); });
    const abort = new AbortController(); const first = f.fs.ready(abort.signal).then(() => 'ready', error => error);
    await entered.promise; const live = f.fs.ready().then(() => 'ready', error => error); abort.abort();
    expect(await first).toMatchObject({ code: 'FS_ABORTED' }); release.resolve();
    expect(await live).toBe('ready'); expect(f.fs.processPath(await f.fs.resolve('a'))).toBe('/workspace/a');
    expect(f.uploads.filter(upload => upload.path.endsWith('/fs_helper.py'))).toHaveLength(1);
  });
  it('still cancels provider-owned preparation on disposal and never launches protocol work after disposal', async () => {
    const f = await fixture(); const entered = deferred<void>(), release = deferred<void>();
    const original = f.files.writeFiles.getMockImplementation()!;
    f.files.writeFiles.mockImplementation(async entries => { if (entries[0]?.path.endsWith('/fs_helper.py')) { entered.resolve(); await release.promise; } await original(entries); });
    const preparing = f.fs.ready().catch(error => error); await entered.promise;
    const disposal = f.fs.dispose(); expect(await preparing).toMatchObject({ code: 'FS_ABORTED' });
    release.resolve(); await disposal; expect(f.requestCount()).toBe(0); expect(await f.artifactRoots()).toHaveLength(0);
  });
  it('does not reset or retry actual unknown helper setup for future waiters', async () => {
    const f = await fixture(); f.files.writeFiles.mockRejectedValue(new Error('private unknown upload'));
    await expect(f.fs.ready()).rejects.toMatchObject({ outcome: 'unknown' }); const count = f.calls.length;
    await expect(f.fs.resolve('a')).rejects.toMatchObject({ outcome: 'unknown' }); expect(f.calls).toHaveLength(count);
    expect(f.files.writeFiles).toHaveBeenCalledTimes(1); expect(f.requestCount()).toBe(0);
  });
  it('preserves the observed command ID and unknown state when mutation observation times out', async () => {
    const f = await fixture(undefined, undefined, { timeoutMs: 100, settlementTimeoutMs: 40 }); const target = await f.fs.resolve('a');
    f.controls.dropTerminal = true; f.controls.hangAfterExit = true; f.commands.getCommandStatus.mockImplementation(() => new Promise(() => undefined));
    const error = await f.fs.writeText(target, 'committed').catch(error => error); const observed = f.commands.interrupt.mock.calls.at(-1)?.[0];
    expect(observed).toBeTruthy(); expect(error).toMatchObject({ code: 'FS_IO_ERROR', commandId: observed, outcome: 'unknown', remoteState: 'unknown' });
    expect(error.cause).toBeUndefined(); expect(await readFile(f.local('/workspace/a'), 'utf8')).toBe('committed');
  });
  it.each(['stat', 'list'] as const)('rejects array-coerced %s metadata enum values', async operation => {
    const f = await fixture(); const target = await f.fs.resolve('.');
    const result = operation === 'stat' ? { version: 'v', type: ['file'], size: 1 }
      : [{ name: 'a', type: ['file'], target: { path: '/workspace/a', displayPath: '/workspace/a' }, version: 'v', size: 1 }];
    f.controls.transformResponse = () => Buffer.from(JSON.stringify({ protocol: 1, ok: true, result }));
    await expect(operation === 'stat' ? f.fs.stat(target) : f.fs.listDir(target)).rejects.toMatchObject({ code: 'FS_IO_ERROR' });
  });
});
