// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { Sandbox } from '@alibaba-group/opensandbox';
import { Context } from '@deepseek-ai/cordis';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { HelperClient } from '../src/helper-client.js';
import { RemoteShell } from '../src/shell.js';
import type { BoundSandbox } from '../src/types.js';

/** Actual published SDK/default adapters, in-memory HTTP and execd's octal-digit interpretation. */
async function wireFixture(openBodies = false) {
  const controllers: ReadableStreamDefaultController<Uint8Array>[] = []; let cancelled = 0;
  const controls = {
    numericError: false, responseError: false, cancel: 'normal' as 'normal' | 'reject' | 'hang',
    pathRecords: undefined as ((path: string) => string[]) | undefined,
    pathExitCode: 0,
  };
  const uploads: { path: string; mode: number; bytes: Uint8Array; bits: number | undefined }[] = [];
  const directories: { path: string; mode: number; bits: number | undefined }[] = [];
  const commandArgv: string[][] = [];
  const files = new Map<string, Uint8Array>(); let requestSerial = 0;
  const bits = (mode: unknown): number | undefined => {
    if (mode === 0) return undefined;
    if (typeof mode !== 'number' || !Number.isSafeInteger(mode) || !/^[0-7]+$/.test(String(mode))) throw new Error('Invalid octal permissions.');
    return Number.parseInt(String(mode), 8);
  };
  const complete = (stdout: string | string[] = '', helper = false, pathRecord = false) => {
    const records = typeof stdout === 'string' ? (stdout ? [stdout] : []) : stdout;
    const exitCode = pathRecord ? controls.pathExitCode : helper && controls.numericError ? 7 : 0;
    const events = [
      { type: 'init', text: `wire-${commandArgv.length}` },
      ...records.map(text => ({ type: 'stdout', text })),
      exitCode !== 0 ? { type: 'error', error: { ename: 'CommandExecError', evalue: String(exitCode) } } : { type: 'execution_complete' },
    ];
    const wire = events.map(event => `data: ${JSON.stringify(event)}\n\n`).join('');
    const body = openBodies ? new ReadableStream<Uint8Array>({
      start(controller) { controllers.push(controller); controller.enqueue(Buffer.from(wire)); },
      cancel() {
        cancelled++;
        if (controls.cancel === 'reject') return Promise.reject(new Error('cleanup failed'));
        if (controls.cancel === 'hang') return new Promise<void>(() => {});
      },
    }) : wire;
    return new Response(body, { headers: { 'content-type': 'text/event-stream' } });
  };
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init); const url = new URL(request.url);
    if (url.pathname.includes('/endpoints/')) return Response.json({ endpoint: 'offline.invalid/mode-wire' }, { headers: { 'OPEN-SANDBOX-ORIGIN': 'template' } });
    if (url.pathname.endsWith('/files/upload')) {
      const form = await request.formData(); const metadata = JSON.parse(await (form.get('metadata') as Blob).text()) as { path: string; mode: number };
      const bytes = new Uint8Array(await (form.get('file') as Blob).arrayBuffer());
      files.set(metadata.path, bytes); // Execd writes bytes before applying permissions.
      let permission: number | undefined; try { permission = bits(metadata.mode); } catch { return Response.json({ code: 'RUNTIME_ERROR' }, { status: 500 }); }
      uploads.push({ ...metadata, bytes, bits: permission }); return new Response(null, { status: 204 });
    }
    if (url.pathname.endsWith('/directories') && request.method === 'POST') {
      const values = await request.json() as Record<string, { mode: number }>;
      for (const [path, permission] of Object.entries(values)) {
        let mode: number | undefined; try { mode = bits(permission.mode); } catch { return Response.json({ code: 'RUNTIME_ERROR' }, { status: 500 }); }
        directories.push({ path, mode: permission.mode, bits: mode });
      }
      return new Response(null, { status: 204 });
    }
    if (url.pathname.endsWith('/command') && request.method === 'POST') {
      const body = await request.json() as { argv: string[] }; commandArgv.push(body.argv); const argv = body.argv;
      if (argv[1]?.endsWith('/fs_helper.py')) {
        const data = JSON.parse(Buffer.from(files.get(argv[2]!)!).toString('utf8')) as { operation: string };
        const result = data.operation === 'resolve' ? { path: '/workspace', displayPath: '/workspace' } : { version: 'wire-version', type: 'directory' };
        files.set(argv[3]!, Buffer.from(JSON.stringify(controls.responseError
          ? { protocol: 1, ok: false, error: { code: 'FS_NOT_FOUND', message: 'missing' } } : { protocol: 1, ok: true, result }))); return complete('', true);
      }
      // Execd commandOutputTail emits nonempty stdout lines without their CR/LF.
      if (argv[2]?.includes('prefix="opensandbox-dsh-"')) {
        const path = '/tmp/opensandbox-dsh-wire'; return complete(controls.pathRecords?.(path) ?? [path], false, true);
      }
      if (argv[2]?.includes('prefix="request-"')) {
        const path = `/tmp/opensandbox-dsh-wire/request-${++requestSerial}`; return complete(controls.pathRecords?.(path) ?? [path], false, true);
      }
      return complete();
    }
    if (url.pathname.endsWith('/files/download')) {
      const bytes = files.get(url.searchParams.get('path')!); if (!bytes) return new Response(null, { status: 404 });
      return new Response(Uint8Array.from(bytes));
    }
    if (request.method === 'DELETE') return new Response(null, { status: 204 });
    throw new Error('Unexpected in-memory SDK wire endpoint.');
  });
  const sandbox = await Sandbox.connect({ sandboxId: 'mode-wire', skipHealthCheck: true, connectionConfig: { domain: 'offline.invalid', disableMetrics: true } });
  const descriptor = Object.freeze({ version: 1 as const, sessionId: 'wire', sandboxId: sandbox.id, remoteCwd: '/workspace' });
  const binding: BoundSandbox = Object.freeze({ descriptor, sandbox, state: 'open' as const, close: () => sandbox.close(), kill: () => sandbox.kill(), toJSON: () => descriptor });
  return { sandbox, binding, uploads, directories, commandArgv, controls, get cancelled() { return cancelled; },
    closeBodies() { for (const controller of controllers) { try { controller.close(); } catch { /* Already cancelled. */ } } },
  };
}
afterEach(() => vi.unstubAllGlobals());

describe('published SDK helper stdout path-record framing', () => {
  it.each(['execd-line', 'explicit-LF'] as const)('accepts exactly one %s path record in bootstrap and request preparation', async framing => {
    const f = await wireFixture(); const helper = new HelperClient(f.binding, { timeoutMs: 500 });
    f.controls.pathRecords = path => [path + (framing === 'explicit-LF' ? '\n' : '')];
    try {
      await helper.ready();
      await expect(helper.request('stat', { target: { path: '/workspace', displayPath: '/workspace' } })).resolves.toMatchObject({ type: 'directory' });
      expect(f.commandArgv).toHaveLength(8);
      expect(f.uploads).toHaveLength(4);
    } finally { await helper.dispose(); await f.sandbox.close(); }
  });
  it('accepts delimiter-free request records after an explicit-LF bootstrap', async () => {
    const f = await wireFixture(); const helper = new HelperClient(f.binding, { timeoutMs: 500 });
    f.controls.pathRecords = path => [path + (path.includes('/request-') ? '' : '\n')];
    try { await helper.ready(); expect(f.uploads).toHaveLength(3); }
    finally { await helper.dispose(); await f.sandbox.close(); }
  });

  const malformed: { name: string; records: (path: string) => string[] }[] = [
    { name: 'absent output', records: () => [] },
    { name: 'empty record', records: () => [''] },
    { name: 'only LF', records: () => ['\n'] },
    { name: 'double trailing LF', records: path => [path + '\n\n'] },
    { name: 'trailing CR', records: path => [path + '\r'] },
    { name: 'trailing CRLF', records: path => [path + '\r\n'] },
    { name: 'embedded LF', records: path => [path.slice(0, -1) + '\n' + path.slice(-1)] },
    { name: 'embedded CR', records: path => [path.slice(0, -1) + '\r' + path.slice(-1)] },
    { name: 'leading space', records: path => [' ' + path] },
    { name: 'trailing space', records: path => [path + ' '] },
    { name: 'leading tab', records: path => ['\t' + path] },
    { name: 'trailing tab', records: path => [path + '\t'] },
    { name: 'NUL', records: path => [path + '\0'] },
    { name: 'control byte', records: path => [path + '\x1b'] },
    { name: 'Unicode separator', records: path => [path + '\u2028'] },
    { name: 'wrong parent', records: path => [path.replace('/tmp/', '/other/')] },
    { name: 'unexpected directory', records: path => [path.includes('/request-')
      ? path.replace('opensandbox-dsh-wire', 'opensandbox-dsh-other') : path.replace('opensandbox-dsh-', 'unexpected-')] },
    { name: 'empty basename', records: path => [path.slice(0, path.lastIndexOf('-') + 1)] },
    { name: 'traversal', records: path => [path + '/../outside'] },
    { name: 'extra child', records: path => [path + '/child'] },
    { name: 'shell injection', records: path => [path + ';true'] },
    { name: 'command substitution', records: path => [path + '$(id)'] },
    { name: 'two path lines', records: path => [path + '\n' + path] },
    { name: 'extra output line', records: path => [path + '\nnoise'] },
    { name: 'two path events', records: path => [path, path] },
    { name: 'split valid path', records: path => [path.slice(0, -1), path.slice(-1)] },
    { name: 'split valid LF path', records: path => [path.slice(0, -1), path.slice(-1) + '\n'] },
    { name: 'extra empty first event', records: path => ['', path] },
    { name: 'extra empty last event', records: path => [path, ''] },
  ];
  describe.each(['bootstrap', 'request'] as const)('%s', stage => {
    it.each(malformed)('rejects $name without uploading into the supplied path', async ({ records }) => {
      const f = await wireFixture(); const helper = new HelperClient(f.binding, { timeoutMs: 500 });
      try {
        if (stage === 'request') { f.controls.pathRecords = path => [path + '\n']; await helper.ready(); }
        const launches = f.commandArgv.length, uploads = f.uploads.length;
        f.controls.pathRecords = records;
        const operation = stage === 'bootstrap' ? helper.ready() : helper.request('stat', { target: { path: '/workspace', displayPath: '/workspace' } });
        await expect(operation).rejects.toMatchObject({ code: 'FS_IO_ERROR', operation: stage === 'bootstrap' ? 'prepare' : 'stat', outcome: 'unknown', remoteState: 'stopped' });
        expect(f.commandArgv).toHaveLength(launches + 1);
        expect(f.uploads).toHaveLength(uploads);
      } finally { await helper.dispose(); await f.sandbox.close(); }
    });
    it('rejects nonzero exit even with one otherwise valid record', async () => {
      const f = await wireFixture(); const helper = new HelperClient(f.binding, { timeoutMs: 500 });
      try {
        if (stage === 'request') { f.controls.pathRecords = path => [path + '\n']; await helper.ready(); }
        f.controls.pathExitCode = 7;
        const operation = stage === 'bootstrap' ? helper.ready() : helper.request('stat', { target: { path: '/workspace', displayPath: '/workspace' } });
        await expect(operation).rejects.toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown', remoteState: 'stopped' });
      } finally { await helper.dispose(); await f.sandbox.close(); }
    });
    it('retains the existing 8192-byte stdout bound', async () => {
      const f = await wireFixture(); const helper = new HelperClient(f.binding, { timeoutMs: 500 });
      try {
        if (stage === 'request') { f.controls.pathRecords = path => [path + '\n']; await helper.ready(); }
        f.controls.pathRecords = path => [path + 'a'.repeat(8193)];
        const operation = stage === 'bootstrap' ? helper.ready() : helper.request('stat', { target: { path: '/workspace', displayPath: '/workspace' } });
        await expect(operation).rejects.toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown' });
      } finally { await helper.dispose(); await f.sandbox.close(); }
    });
  });
});

describe('published SDK private-artifact permission wire contract', () => {
  it('uploads helper and JSON through real multipart metadata with mode 600 and passes the readiness protocol', async () => {
    const f = await wireFixture(); const helper = new HelperClient(f.binding, { timeoutMs: 500 });
    try {
      await helper.ready();
      expect(f.uploads.map(upload => upload.mode)).toEqual([600, 600, 600]);
      expect(f.uploads.every(upload => upload.bits === 0o600)).toBe(true);
      expect(f.uploads.filter(upload => upload.path.endsWith('/request.json')).map(upload => JSON.parse(Buffer.from(upload.bytes).toString('utf8')).operation)).toEqual(['resolve', 'stat']);
    } finally { await helper.dispose(); await f.sandbox.close(); }
  });
  it.each(['success', 'numeric-error'] as const)('returns real SDK SSE iterators after %s even when terminal bodies stay open', async terminal => {
    const f = await wireFixture(true); const helper = new HelperClient(f.binding, { timeoutMs: 500, settlementTimeoutMs: 20 });
    try {
      await helper.ready();
      f.controls.numericError = terminal === 'numeric-error';
      const request = helper.request('stat', { target: { path: '/workspace', displayPath: '/workspace' } });
      if (terminal === 'success') await expect(request).resolves.toMatchObject({ type: 'directory', version: 'wire-version' });
      else await expect(request).rejects.toMatchObject({ code: 'FS_IO_ERROR', outcome: 'unknown', remoteState: 'stopped' });
      await helper.dispose();
      expect(f.commandArgv).toHaveLength(8); expect(f.cancelled).toBe(8);
    } finally { f.closeBodies(); await helper.dispose(); await f.sandbox.close(); }
  });
  it.each(['reject', 'hang'] as const)('bounds terminal iterator cleanup that can %s without replacing authoritative filesystem outcomes', async cleanup => {
    const f = await wireFixture(true); const helper = new HelperClient(f.binding, { timeoutMs: 500, settlementTimeoutMs: 20 });
    try {
      await helper.ready(); f.controls.cancel = cleanup;
      const request = () => helper.request('stat', { target: { path: '/workspace', displayPath: '/workspace' } });
      await expect(request()).resolves.toMatchObject({ type: 'directory', version: 'wire-version' });
      f.controls.responseError = true;
      await expect(request()).rejects.toMatchObject({ code: 'FS_NOT_FOUND', remoteState: 'stopped' });
      await helper.dispose(); expect(f.cancelled).toBe(10);
    } finally { f.closeBodies(); await helper.dispose(); await f.sandbox.close(); }
  });
  it('stages shell stdin via real directory JSON mode 700 and multipart file mode 600 before command launch', async () => {
    const f = await wireFixture(); const shell = new RemoteShell(new Context(), { binding: f.binding });
    const stdin = 'exact\0世界\n$(literal)';
    try {
      const handle = await shell.execute(shell.resolve({ command: 'cat', stdin })); await handle.done;
      expect((await handle.result()).exitCode).toBe(0);
      expect(f.directories).toHaveLength(1); expect(f.directories[0]).toMatchObject({ mode: 700, bits: 0o700 });
      expect(f.uploads).toHaveLength(1); expect(f.uploads[0]).toMatchObject({ mode: 600, bits: 0o600 });
      expect(f.uploads[0]!.bytes).toEqual(new Uint8Array(Buffer.from(stdin)));
      expect(f.commandArgv[0]?.[3]).toBe(f.uploads[0]?.path);
    } finally { await shell.dispose(); await f.sandbox.close(); }
  });
});
