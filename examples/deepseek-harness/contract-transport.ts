// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';
import type { ExecdCommands, Sandbox, SandboxFiles, ServerStreamEvent } from '@alibaba-group/opensandbox';
import type { BindingState, BoundSandbox } from '@opensandbox/deepseek-harness';

export const SMOKE_COMMAND = "printf '%s' 'contract' > note.txt";

/** Explicit local contract simulation, never imported by the real entry.
 * NOT a security sandbox. Only trusted fixed smoke code may use this transport.
 * The helper is real Python; paths are mapped into disposable local directories.
 */
export async function createContractTransport() {
  const root = await mkdtemp(join(tmpdir(), 'dsh-example-contract-'));
  await mkdir(join(root, 'workspace')); await mkdir(join(root, 'tmp'));
  const processes = new Map<string, {child: ReturnType<typeof spawn>; running: boolean; exitCode: number | null; done: Promise<void>}>();
  let serial = 0, state: BindingState = 'open';
  const local = (path: string) => {
    assert(path === '/workspace' || path.startsWith('/workspace/') || path.startsWith('/tmp/'), 'Only the explicit contract namespace is supported');
    assert(!path.split('/').includes('..'), 'Parent traversal is outside the smoke contract');
    return root + path;
  };
  const remote = (path: string) => { assert(path.startsWith(root + '/')); return path.slice(root.length); };
  const tree = (value: unknown, map: (path: string) => string): unknown => {
    if (Array.isArray(value)) return value.map(item => tree(item, map));
    if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([key, item]) => [key,
      ['path', 'displayPath', 'cwd', 'cancelPath'].includes(key) && typeof item === 'string' ? (item.startsWith('/') ? map(item) : item) : tree(item, map)]));
    return value;
  };
  const bits = (mode?: number) => {
    if (mode === undefined) return undefined;
    assert(/^[0-7]+$/.test(String(mode)), 'SDK modes use octal digits, e.g. 600/700');
    return Number.parseInt(String(mode), 8);
  };
  const files = {
    async createDirectories(entries) { for (const entry of entries) await mkdir(local(entry.path), {recursive: true, mode: bits(entry.mode)}); },
    async writeFiles(entries) {
      for (const entry of entries) {
        assert(entry.data instanceof Uint8Array, 'Smoke only writes explicit bytes');
        const bytes = entry.path.endsWith('/request.json')
          ? Buffer.from(JSON.stringify(tree(JSON.parse(Buffer.from(entry.data).toString('utf8')), local))) : entry.data;
        await writeFile(local(entry.path), bytes, {mode: bits(entry.mode)});
      }
    },
    async readBytes(path) { return new Uint8Array(await readFile(local(path))); },
    async *readBytesStream(path) {
      let bytes: Uint8Array = await readFile(local(path));
      if (path.endsWith('/response.json')) bytes = Buffer.from(JSON.stringify(tree(JSON.parse(Buffer.from(bytes).toString('utf8')), remote)));
      for (let offset = 0; offset < bytes.length; offset += 3) yield bytes.subarray(offset, offset + 3);
    },
    async deleteDirectories(paths) {for (const path of paths) await rm(local(path), {recursive: true, force: true});},
    async deleteFiles(paths) {for (const path of paths) await rm(local(path), {force: true});},
  } satisfies Pick<SandboxFiles, 'createDirectories' | 'writeFiles' | 'readBytes' | 'readBytesStream' | 'deleteDirectories' | 'deleteFiles'>;
  const commands = {
    async *runStream(input, options) {
      assert(Array.isArray(input) && input[0] === 'python3', 'Smoke only runs the packaged Python preparation/helper/wrapper');
      const argv = [...input];
      if (argv[1] === '-c') {
        const code = argv[2]!;
        const bootstrap = argv.length === 3 && code.includes('mkdtemp(prefix="opensandbox-dsh-",dir="/tmp")');
        const requestDir = argv.length === 4 && code.includes('mkdtemp(prefix="request-",dir=p)');
        const verify = argv.length === 6 && code.includes('hashlib.sha256(data).hexdigest() == sys.argv[2]');
        const shell = argv.length === 7 && argv[4] === SMOKE_COMMAND && argv[6] === '/workspace' && code.includes('os.execvpe("bash"');
        assert(bootstrap || requestDir || verify || shell, 'Unrecognized command is outside the fixed smoke contract');
      } else assert(argv.length === 4 && argv[1]?.endsWith('/fs_helper.py'), 'Only packaged helper requests are supported');
      const args = argv.slice(1).map(arg => arg.startsWith('/workspace') || arg.startsWith('/tmp/') ? local(arg) : arg);
      if (args[0] === '-c') args[1] = args[1]!.replace('dir="/tmp"', `dir=${JSON.stringify(join(root, 'tmp'))}`);
      const child = spawn('python3', args, {env: {PATH: process.env.PATH, TMPDIR: join(root, 'tmp'), ...options?.envs}});
      const id = `contract-${++serial}`;
      const events: ServerStreamEvent[] = [];
      let wake: (() => void) | undefined;
      const push = (event: ServerStreamEvent) => {events.push(event); wake?.(); wake = undefined;};
      let finish!: () => void;
      const status = {child, running: true, exitCode: null as number | null, done: new Promise<void>(resolve => {finish = resolve;})};
      processes.set(id, status);
      // Model execd's delimiter-free nonempty line events for the fixed smoke commands.
      createInterface({input: child.stdout, crlfDelay: Infinity}).on('line', line =>
        push({type: 'stdout', text: (line || '\n').replaceAll(root + '/', '/')}));
      child.stderr.on('data', () => push({type: 'stderr', text: 'Local contract subprocess diagnostic omitted.'}));
      child.on('error', () => push({type: 'error', error: {ename: 'CommandExecError', evalue: '1'}}));
      child.on('close', code => {
        status.running = false; status.exitCode = code;
        push(code === 0 ? {type: 'execution_complete'} : {type: 'error', error: {ename: 'CommandExecError', evalue: String(code ?? 1)}});
        finish();
      });
      yield {type: 'init', text: id};
      while (status.running || events.length) {
        if (!events.length) await new Promise<void>(resolve => {wake = resolve;});
        while (events.length) yield events.shift()!;
      }
      await status.done;
    },
    async interrupt(id) {processes.get(id)?.child.kill('SIGTERM');},
    async getCommandStatus(id) {
      const found = processes.get(id); assert(found, 'Unknown contract command');
      return {id, running: found.running, exitCode: found.exitCode};
    },
  } satisfies Pick<ExecdCommands, 'runStream' | 'interrupt' | 'getCommandStatus'>;
  // Only the SDK surface consumed by the providers is implemented at this explicit boundary.
  const sandbox = {id: 'contract-only-sandbox', commands, files} as unknown as Sandbox;
  const descriptor = Object.freeze({version: 1 as const, sessionId: 'contract-only-session', sandboxId: sandbox.id, remoteCwd: '/workspace'});
  const binding: BoundSandbox = Object.freeze({descriptor, sandbox,
    get state() {return state;}, close: async () => {state = 'closed';}, kill: async () => {state = 'killed';}, toJSON: () => descriptor});
  return {binding, readText: async () => readFile(local('/workspace/note.txt'), 'utf8'),
    cleanup: async () => {
      for (const item of processes.values()) if (item.running) item.child.kill('SIGKILL');
      await Promise.all([...processes.values()].map(item => item.done));
      await rm(root, {recursive: true, force: true});
    },
  };
}
