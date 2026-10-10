// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Sandbox, ServerStreamEvent } from '@alibaba-group/opensandbox';
import type { BindingState, BoundSandbox } from '../../src/types.js';
import { fakeSandbox } from './sdk-facade.js';
import { execdLines } from './execd-lines.js';

/** Test-only remote namespace boundary. Actual Python runs only in disposable local directories. */
export async function helperSdk(id = 'sandbox-a', sessionId = 'session-a') {
  const root = await mkdtemp(join(tmpdir(), 'dsh-helper-sdk-'));
  await mkdir(join(root, 'workspace')); await mkdir(join(root, 'tmp'));
  const sdk = fakeSandbox(id);
  const processes = new Map<string, { child: ReturnType<typeof spawn>; running: boolean; exitCode: number | null }>();
  const calls: { argv: string[]; options: unknown }[] = [];
  const uploads: { path: string; data: Uint8Array; mode?: number }[] = [];
  let state: BindingState = 'open';
  let serial = 0;
  const local = (path: string) => path.startsWith('/workspace') ? root + path : path.startsWith('/tmp/') ? root + path : path;
  const remote = (path: string) => path.startsWith(root + '/') ? path.slice(root.length) : path;
  // Published SDK mode numbers are octal digits. Execd parses decimal text in base 8.
  const permissionBits = (mode?: number): number | undefined => {
    if (mode === undefined || mode === 0) return undefined;
    if (!Number.isSafeInteger(mode) || !/^[0-7]+$/.test(String(mode))) throw new Error('Invalid execd octal-digit mode.');
    return Number.parseInt(String(mode), 8);
  };
  const tree = (value: unknown, map: (path: string) => string): unknown => {
    if (Array.isArray(value)) return value.map(item => tree(item, map));
    if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key,
      ['path', 'displayPath', 'cwd', 'cancelPath'].includes(key) && typeof item === 'string' ? map(item) : tree(item, map)]));
    return value;
  };
  const controls = {
    transformResponse: undefined as ((bytes: Uint8Array, path: string) => Uint8Array) | undefined,
    beforeLaunch: undefined as ((argv: string[]) => void | Promise<void>) | undefined,
    afterLaunch: undefined as ((argv: string[], commandId: string) => void | Promise<void>) | undefined,
    afterExit: undefined as ((argv: string[], commandId: string) => void | Promise<void>) | undefined,
    dropInit: false, dropTerminal: false, hangAfterExit: false, failBeforeInit: false,
    downloadChunk: 8192,
    pythonOptimize: '0' as '0' | '1' | '2',
  };
  sdk.files.createDirectories.mockImplementation(async entries => { for (const entry of entries) await mkdir(local(entry.path), { recursive: true, mode: permissionBits(entry.mode) }); });
  sdk.files.writeFiles.mockImplementation(async entries => {
    for (const entry of entries) {
      if (!(entry.data instanceof Uint8Array)) throw new Error('Test boundary requires bytes.');
      uploads.push({ path: entry.path, data: Uint8Array.from(entry.data), ...(entry.mode !== undefined ? { mode: entry.mode } : {}) });
      const data = entry.path.endsWith('/request.json')
        ? Buffer.from(JSON.stringify(tree(JSON.parse(Buffer.from(entry.data).toString('utf8')), local))) : entry.data;
      await writeFile(local(entry.path), data, { mode: permissionBits(entry.mode) });
    }
  });
  sdk.files.readBytesStream.mockImplementation(async function* (path) {
    let data: Uint8Array = await readFile(local(path));
    if (path.endsWith('/response.json')) data = Buffer.from(JSON.stringify(tree(JSON.parse(Buffer.from(data).toString('utf8')), remote)));
    data = controls.transformResponse?.(data, path) ?? data;
    for (let offset = 0; offset < data.byteLength; offset += controls.downloadChunk) yield data.subarray(offset, offset + controls.downloadChunk);
  });
  sdk.files.deleteDirectories.mockImplementation(async paths => { for (const path of paths) await rm(local(path), { force: true, recursive: true }); });
  sdk.files.deleteFiles.mockImplementation(async paths => { for (const path of paths) await rm(local(path), { force: true }); });
  sdk.commands.runStream.mockImplementation(async function* (input, options) {
    if (!Array.isArray(input)) throw new Error('Test boundary requires argv.');
    const argv = [...input]; calls.push({ argv, options });
    const helper = argv[1]?.endsWith('/fs_helper.py') ?? false;
    await controls.beforeLaunch?.(argv);
    if (controls.failBeforeInit) throw new Error('private token: never show');
    const commandId = `helper-${++serial}`;
    const processArgv = argv.slice(1).map(local);
    // The remote /tmp mount exists only at this test boundary, just like /workspace.
    if (processArgv[0] === '-c') processArgv[1] = processArgv[1]!.replace('dir="/tmp"', `dir=${JSON.stringify(join(root, 'tmp'))}`);
    const child = spawn(argv[0]!, processArgv, { env: { PATH: process.env.PATH, TMPDIR: join(root, 'tmp'), PYTHONOPTIMIZE: controls.pythonOptimize } });
    const processState = { child, running: true, exitCode: null as number | null }; processes.set(commandId, processState);
    const events: ServerStreamEvent[] = []; let wake: (() => void) | undefined;
    const push = (event: ServerStreamEvent) => { events.push(event); wake?.(); wake = undefined; };
    const stdout = execdLines(text => push({ type: 'stdout', text: text.replaceAll(root + '/', '/') }));
    child.stdout.on('data', bytes => stdout.write(bytes)); child.stdout.on('end', () => stdout.end());
    child.stderr.on('data', bytes => push({ type: 'stderr', text: Buffer.from(bytes).toString('utf8') }));
    child.on('error', () => { processState.running = false; push({ type: 'error', error: { ename: 'CommandExecError', evalue: '1' } }); });
    const exited = new Promise<void>(resolve => child.on('close', (code) => {
      processState.running = false; processState.exitCode = code;
      void (async () => {
        await controls.afterExit?.(argv, commandId);
        if (!helper || !controls.dropTerminal) push(code === 0 ? { type: 'execution_complete' } : { type: 'error', error: { ename: 'CommandExecError', evalue: String(code ?? 1) } });
        resolve(); wake?.(); wake = undefined;
      })();
    }));
    if (!helper || !controls.dropInit) yield { type: 'init', text: commandId };
    await controls.afterLaunch?.(argv, commandId);
    while (processState.running || events.length) {
      if (!events.length) await new Promise<void>(resolve => { wake = resolve; });
      while (events.length) yield events.shift()!;
    }
    await exited;
    while (events.length) yield events.shift()!;
    if (helper && controls.hangAfterExit) await new Promise<void>(() => undefined);
  });
  sdk.commands.interrupt.mockImplementation(async id => { processes.get(id)?.child.kill('SIGTERM'); });
  sdk.commands.getCommandStatus.mockImplementation(async id => {
    const found = processes.get(id); if (!found) throw new Error('unknown command');
    return { id, running: found.running, exitCode: found.exitCode };
  });
  const descriptor = Object.freeze({ version: 1 as const, sessionId, sandboxId: id, remoteCwd: '/workspace' });
  const binding: BoundSandbox = Object.freeze({ descriptor, sandbox: sdk.sandbox as Sandbox,
    get state() { return state; }, close: async () => { state = 'closed'; }, kill: async () => { state = 'killed'; }, toJSON: () => descriptor });
  return { ...sdk, binding, root, calls, uploads, controls, local,
    requestCount: () => calls.filter(call => call.argv[1]?.endsWith('/fs_helper.py')).length,
    artifactRoots: () => readdir(join(root, 'tmp')),
    cleanup: async () => { for (const item of processes.values()) if (item.running) item.child.kill('SIGKILL'); await rm(root, { recursive: true, force: true }); },
  };
}
