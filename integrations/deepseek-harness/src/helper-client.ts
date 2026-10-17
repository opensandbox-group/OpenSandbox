// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { posix } from 'node:path';
import { FsError } from '@deepseek-ai/dsh-fs';
import type { FsErrorCode } from '@deepseek-ai/dsh-fs';
import { SdkTransport } from './sdk-transport.js';
import type { BoundSandbox } from './types.js';

export const MAX_DATA_BYTES = 1024 * 1024;
export const MAX_REQUEST_BYTES = 8 * 1024 * 1024;
export const MAX_RESPONSE_BYTES = 16 * 1024 * 1024;
const MAX_PATH_BYTES = 4096;
const MAX_LIST_ENTRIES = 4096;
const MAX_TIMER_MS = 2_147_483_647;

export interface HelperTarget { path: string; displayPath: string; }
export interface HelperInfo { version: string; type: 'file' | 'directory' | 'other'; size?: number; }
export interface HelperPathInfo extends Omit<HelperInfo, 'type'> { type: HelperInfo['type'] | 'symlink'; }
export interface HelperEntry { name: string; type: HelperInfo['type']; target: HelperTarget; version?: string; size?: number; }
export type HelperOperation = 'resolve' | 'lstat' | 'stat' | 'list' | 'read-text' | 'read-bytes' | 'read-range' | 'write-text' | 'edit-text';
type WriteIntent = { kind: 'createIfAbsent' } | { kind: 'replaceIfVersion'; version: string };
interface HelperArgs {
  resolve: { path: string; cwd: string };
  lstat: { path: string; cwd: string };
  stat: { target: HelperTarget };
  list: { target: HelperTarget };
  'read-text': { target: HelperTarget };
  'read-bytes': { target: HelperTarget; maxBytes: number };
  'read-range': { target: HelperTarget; offset: number; length: number };
  'write-text': { target: HelperTarget; content: string; expected?: WriteIntent };
  'edit-text': { target: HelperTarget; edit: { oldString: string; newString: string; replaceAll: boolean }; expected?: { version: string } };
}
interface HelperResults {
  resolve: HelperTarget;
  lstat: HelperPathInfo | null;
  stat: HelperInfo | null;
  list: HelperEntry[];
  'read-text': { text: string; version: string };
  'read-bytes': { base64: string; version: string };
  'read-range': { base64: string; version: string };
  'write-text': { operation: 'create' | 'update'; version: string; before: string | null; after: string };
  'edit-text': { version: string; before: string; after: string };
}
export interface HelperClientOptions {
  /** Finite preparation/transfer/command budget, separate from sandbox TTL. */
  timeoutMs?: number;
  /** Bounded observation after abort, disconnect or timeout. */
  settlementTimeoutMs?: number;
}
interface FsDiagnostic {
  operation?: HelperOperation | 'prepare'; sandboxId?: string; commandId?: string;
  outcome?: 'known' | 'unknown'; remoteState?: 'stopped' | 'unknown';
}
const MESSAGES: Record<FsErrorCode, string> = {
  FS_NOT_FOUND: 'Remote filesystem path was not found.',
  FS_NOT_DIRECTORY: 'Remote filesystem target is not a directory.',
  FS_NOT_TEXT: 'Remote content is not regular UTF-8 text without NUL.',
  FS_NOT_REGULAR_FILE: 'Remote filesystem target is not a regular file.',
  FS_TOO_LARGE: 'Remote filesystem path, content or response exceeds its finite limit.',
  FS_PERMISSION_DENIED: 'Remote filesystem permission denied.',
  FS_SANDBOX_DENIED: 'The requested filesystem policy or session cannot be enforced by this binding.',
  FS_IO_ERROR: 'Remote filesystem operation failed.',
  FS_STALE_VERSION: 'Remote file changed since it was observed.',
  FS_NOT_OBSERVED: 'An existing remote file cannot be replaced by createIfAbsent.',
  FS_AMBIGUOUS_EDIT: 'The literal edit matches more than once.',
  FS_EDIT_NOT_FOUND: 'The non-empty literal edit was not found.',
  FS_ABORTED: 'Remote filesystem operation was cancelled before publication.',
};

/** Typed, deliberately sanitized diagnostics: no content, SDK causes, credentials or URLs. */
export class RemoteFsError extends FsError {
  readonly operation: FsDiagnostic['operation']; readonly sandboxId: string | undefined;
  readonly commandId: string | undefined; readonly outcome: 'known' | 'unknown';
  readonly remoteState: 'stopped' | 'unknown';
  constructor(code: FsErrorCode, diagnostic: FsDiagnostic = {}) {
    super(diagnostic.outcome === 'unknown'
      ? 'Remote filesystem outcome is unknown. No retry was attempted; request artifacts were retained.' : MESSAGES[code], code);
    this.operation = diagnostic.operation; this.sandboxId = diagnostic.sandboxId; this.commandId = diagnostic.commandId;
    this.outcome = diagnostic.outcome ?? 'known'; this.remoteState = diagnostic.remoteState ?? 'unknown';
  }
}
export function typedFsError(code: FsErrorCode, diagnostic: FsDiagnostic = {}): RemoteFsError {
  return new RemoteFsError(code, diagnostic);
}
function fail(code: FsErrorCode = 'FS_IO_ERROR'): never { throw typedFsError(code); }
export function assertObject(value: unknown, required: readonly string[], optional: readonly string[] = []): asserts value is Record<string, unknown> {
  try {
    if (!value || typeof value !== 'object' || Array.isArray(value)) fail();
    const prototype = Object.getPrototypeOf(value);
    if (prototype !== Object.prototype && prototype !== null) fail();
    const fields = Object.getOwnPropertyDescriptors(value);
    if (!required.every(key => Object.hasOwn(fields, key)) || Reflect.ownKeys(value).some(key =>
      typeof key !== 'string' || (!required.includes(key) && !optional.includes(key)) ||
      !Object.hasOwn(fields[key]!, 'value') || fields[key]!.enumerable !== true)) fail();
  } catch { fail(); }
}
/** Reject unpaired UTF-16 instead of silently encoding replacement characters. */
export function assertString(value: unknown, cap: number, text = false): asserts value is string {
  if (typeof value !== 'string') fail();
  for (let i = 0; i < value.length; i++) {
    const unit = value.charCodeAt(i);
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(++i); if (!(next >= 0xdc00 && next <= 0xdfff)) fail();
    } else if (unit >= 0xdc00 && unit <= 0xdfff) fail();
  }
  if (Buffer.byteLength(value, 'utf8') > cap) fail('FS_TOO_LARGE');
  if (value.includes('\0')) fail(text ? 'FS_NOT_TEXT' : 'FS_IO_ERROR');
}
export function assertInteger(value: unknown, cap = Number.MAX_SAFE_INTEGER): asserts value is number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) fail();
  if (value > cap) fail('FS_TOO_LARGE');
}
function assertPath(value: unknown, absolute = false): asserts value is string {
  assertString(value, MAX_PATH_BYTES); if (absolute && !value.startsWith('/')) fail();
}
function assertTarget(value: unknown): asserts value is HelperTarget {
  assertObject(value, ['path', 'displayPath']); assertPath(value.path, true); assertPath(value.displayPath, true);
  if (posix.normalize(value.path) !== value.path) fail();
}
function validateArgs(operation: HelperOperation, value: unknown): void {
  if (operation === 'resolve' || operation === 'lstat') {
    assertObject(value, ['path', 'cwd']); assertPath(value.path); assertPath(value.cwd, true);
    if (!value.path.trim()) fail('FS_NOT_FOUND'); return;
  }
  if (operation === 'stat' || operation === 'list' || operation === 'read-text') {
    assertObject(value, ['target']); assertTarget(value.target); return;
  }
  if (operation === 'read-bytes') {
    assertObject(value, ['target', 'maxBytes']); assertTarget(value.target); assertInteger(value.maxBytes, MAX_DATA_BYTES); return;
  }
  if (operation === 'read-range') {
    assertObject(value, ['target', 'offset', 'length']); assertTarget(value.target);
    assertInteger(value.offset); assertInteger(value.length, MAX_DATA_BYTES);
    if (!Number.isSafeInteger(value.offset + value.length)) fail(); return;
  }
  if (operation !== 'write-text' && operation !== 'edit-text') fail();
  assertObject(value, ['target', operation === 'write-text' ? 'content' : 'edit'], ['expected']); assertTarget(value.target);
  if (operation === 'write-text') {
    assertString(value.content, MAX_DATA_BYTES, true);
    if (value.expected !== undefined) {
      assertObject(value.expected, ['kind'], ['version']);
      if (value.expected.kind === 'createIfAbsent') assertObject(value.expected, ['kind']);
      else if (value.expected.kind === 'replaceIfVersion') { assertObject(value.expected, ['kind', 'version']); assertString(value.expected.version, 256); }
      else fail();
    }
  } else {
    assertObject(value.edit, ['oldString', 'newString', 'replaceAll']);
    assertString(value.edit.oldString, MAX_DATA_BYTES, true); assertString(value.edit.newString, MAX_DATA_BYTES, true);
    if (!value.edit.oldString) fail('FS_EDIT_NOT_FOUND'); if (typeof value.edit.replaceAll !== 'boolean') fail();
    if (value.expected !== undefined) { assertObject(value.expected, ['version']); assertString(value.expected.version, 256); }
  }
}

/** A bounded strict JSON parser also rejects duplicate keys and non-integer protocol numbers. */
function parseResponse(text: string): unknown {
  let offset = 0; let budget = 100_000; // Finite list schema can contain 4096 metadata/target objects.
  const whitespace = () => { while (/[\x20\t\r\n]/.test(text[offset] ?? 'x')) offset++; };
  const string = (): string => {
    const start = offset++; let escaped = false;
    while (offset < text.length) {
      const char = text[offset++]!;
      if (!escaped && char === '"') {
        let value: unknown; try { value = JSON.parse(text.slice(start, offset)); } catch { fail(); }
        assertString(value, MAX_RESPONSE_BYTES); return value;
      }
      if (escaped) escaped = false; else if (char === '\\') escaped = true;
    }
    return fail();
  };
  const value = (depth: number): unknown => {
    if (depth > 16 || --budget < 0) fail(); whitespace(); const char = text[offset];
    if (char === '"') return string();
    if (char === '{') {
      offset++; whitespace(); const result: Record<string, unknown> = Object.create(null) as Record<string, unknown>;
      if (text[offset] === '}') { offset++; return result; }
      for (;;) {
        whitespace(); if (text[offset] !== '"') fail(); const key = string(); whitespace();
        if (text[offset++] !== ':' || Object.hasOwn(result, key)) fail(); result[key] = value(depth + 1); whitespace();
        const next = text[offset++]; if (next === '}') return result; if (next !== ',') fail();
      }
    }
    if (char === '[') {
      offset++; whitespace(); const result: unknown[] = []; if (text[offset] === ']') { offset++; return result; }
      for (;;) { result.push(value(depth + 1)); whitespace(); const next = text[offset++]; if (next === ']') return result; if (next !== ',') fail(); }
    }
    for (const [word, result] of [['true', true], ['false', false], ['null', null]] as const) {
      if (text.startsWith(word, offset)) { offset += word.length; return result; }
    }
    const match = /^-?(?:0|[1-9]\d*)/.exec(text.slice(offset)); if (!match) fail();
    offset += match[0].length; const number = Number(match[0]); if (!Number.isSafeInteger(number)) fail(); return number;
  };
  const result = value(0); whitespace(); if (offset !== text.length) fail(); return result;
}
function validateInfo(value: unknown, nofollow = false): void {
  if (value === null) return;
  assertObject(value, ['version', 'type'], ['size']); assertString(value.version, 256);
  if (typeof value.type !== 'string' || !['file', 'directory', 'other', ...(nofollow ? ['symlink'] : [])].includes(value.type)) fail();
  if (value.size !== undefined) assertInteger(value.size);
  if (value.type === 'file' && value.size === undefined) fail();
}
function codepointCompare(a: string, b: string): number {
  const left = Array.from(a), right = Array.from(b);
  for (let i = 0; i < Math.min(left.length, right.length); i++) {
    const diff = left[i]!.codePointAt(0)! - right[i]!.codePointAt(0)!; if (diff) return diff;
  }
  return left.length - right.length;
}
export function decodeBase64(value: string, cap: number): Uint8Array {
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value)) fail();
  if (value.length > 4 * Math.ceil(cap / 3)) fail('FS_TOO_LARGE');
  const bytes = Buffer.from(value, 'base64'); if (bytes.length > cap) fail('FS_TOO_LARGE');
  if (bytes.toString('base64') !== value) fail(); return new Uint8Array(bytes);
}
function validateResult(operation: HelperOperation, result: unknown, args: unknown): void {
  if (operation === 'resolve') { assertTarget(result); return; }
  if (operation === 'stat' || operation === 'lstat') { validateInfo(result, operation === 'lstat'); return; }
  if (operation === 'list') {
    if (!Array.isArray(result)) fail(); if (result.length > MAX_LIST_ENTRIES) fail('FS_TOO_LARGE');
    let prior: string | undefined;
    for (const entry of result) {
      assertObject(entry, ['name', 'type', 'target'], ['version', 'size']); assertString(entry.name, MAX_PATH_BYTES);
      if (!entry.name || entry.name.includes('/') || entry.name === '.' || entry.name === '..' || (prior !== undefined && codepointCompare(prior, entry.name) >= 0)) fail();
      prior = entry.name; assertTarget(entry.target);
      if (typeof entry.type !== 'string' || !['file', 'directory', 'other'].includes(entry.type)) fail();
      if (entry.version !== undefined) assertString(entry.version, 256); if (entry.size !== undefined) assertInteger(entry.size);
    }
    return;
  }
  if (operation === 'read-text') {
    assertObject(result, ['version', 'text']); assertString(result.version, 256); assertString(result.text, MAX_DATA_BYTES, true); return;
  }
  if (operation === 'read-bytes' || operation === 'read-range') {
    assertObject(result, ['version', 'base64']); assertString(result.version, 256); assertString(result.base64, 4 * Math.ceil(MAX_DATA_BYTES / 3));
    const cap = operation === 'read-bytes' ? (args as HelperArgs['read-bytes']).maxBytes : (args as HelperArgs['read-range']).length;
    decodeBase64(result.base64, cap); return;
  }
  assertObject(result, operation === 'write-text' ? ['operation', 'version', 'before', 'after'] : ['version', 'before', 'after']);
  assertString(result.version, 256); assertString(result.after, MAX_DATA_BYTES, true);
  if (operation === 'write-text') {
    if (result.operation !== 'create' && result.operation !== 'update') fail();
    if (result.before !== null) assertString(result.before, MAX_DATA_BYTES, true);
  } else assertString(result.before, MAX_DATA_BYTES, true);
}

const BOOTSTRAP = [
  'import os,sys,tempfile', 'if not (sys.platform == "linux" and sys.version_info >= (3,8)): raise RuntimeError("Unsupported helper runtime")',
  'd=tempfile.mkdtemp(prefix="opensandbox-dsh-",dir="/tmp")', 'os.chmod(d,0o700)', 'print(d,flush=True)',
].join('\n');
const REQUEST_DIRECTORY = [
  'import os,sys,tempfile,stat', 'p=sys.argv[1]', 's=os.lstat(p)',
  'if not (stat.S_ISDIR(s.st_mode) and s.st_uid == os.getuid() and stat.S_IMODE(s.st_mode) == 0o700): raise RuntimeError("Invalid helper directory")',
  'd=tempfile.mkdtemp(prefix="request-",dir=p)', 'os.chmod(d,0o700)', 'print(d,flush=True)',
].join('\n');
const VERIFY_HELPER = [
  'import os,sys,hashlib,stat', 'f=os.open(sys.argv[1],os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK)',
  's=os.fstat(f)', 'if not (stat.S_ISREG(s.st_mode) and s.st_uid == os.getuid() and stat.S_IMODE(s.st_mode) == 0o600): raise RuntimeError("Invalid helper file")',
  'if s.st_size != int(sys.argv[3]): raise RuntimeError("Invalid helper size")', 'data=b""',
  'while len(data) <= int(sys.argv[3]):', ' chunk=os.read(f,min(65536,int(sys.argv[3])+1-len(data)))',
  ' if not chunk: break', ' data+=chunk', 'os.close(f)',
  'if not (len(data) == int(sys.argv[3]) and hashlib.sha256(data).hexdigest() == sys.argv[2]): raise RuntimeError("Invalid helper digest")',
].join('\n');
function timerOption(value: number, cap = MAX_TIMER_MS): number { if (!Number.isSafeInteger(value) || value <= 0 || value > cap) fail(); return value; }
function abortCheck(signal?: AbortSignal): void { if (signal?.aborted) fail('FS_ABORTED'); }
interface HelperCommandResult { stdout: string; stdoutEvents: number; exitCode: number | null; commandId?: string; }
function singleStdoutRecord(result: HelperCommandResult): string | undefined {
  if (result.exitCode !== 0 || result.stdoutEvents !== 1) return undefined;
  // Execd removes nonempty line delimiters. Also permit one explicit transport LF.
  // Preserve event boundaries: concatenating multiple events cannot establish one record.
  const record = result.stdout.endsWith('\n') ? result.stdout.slice(0, -1) : result.stdout;
  return record && !/[\r\n]/.test(record) ? record : undefined;
}

/** Only the bound published SDK is used for target I/O; local I/O loads one fixed packaged code asset. */
export class HelperClient {
  readonly #binding: BoundSandbox; readonly #transport: SdkTransport;
  readonly #sandboxId: string; readonly #sessionId: string; readonly #cwd: string;
  readonly #timeout: number; readonly #settlement: number;
  readonly #shutdown = new AbortController(); readonly #active = new Set<Promise<unknown>>();
  #ready: Promise<void> | undefined; #directory: string | undefined;
  #disposed = false; #retained = false; #disposing: Promise<void> | undefined;

  constructor(binding: BoundSandbox, options: HelperClientOptions = {}) {
    const descriptor = binding.descriptor; assertString(descriptor.sessionId, 4096); assertString(descriptor.sandboxId, 4096); assertPath(descriptor.remoteCwd, true);
    if (descriptor.version !== 1 || !descriptor.sessionId || !descriptor.sandboxId || binding.sandbox.id !== descriptor.sandboxId) fail();
    this.#binding = binding; this.#transport = new SdkTransport(binding.sandbox);
    this.#sandboxId = descriptor.sandboxId; this.#sessionId = descriptor.sessionId; this.#cwd = descriptor.remoteCwd;
    this.#timeout = timerOption(options.timeoutMs ?? 30_000); this.#settlement = timerOption(options.settlementTimeoutMs ?? 3_000);
  }
  get identity(): Readonly<{ sandboxId: string; sessionId: string }> { return Object.freeze({ sandboxId: this.#sandboxId, sessionId: this.#sessionId }); }
  #assertOpen(): void { if (this.#disposed || this.#binding.state !== 'open') fail(); }
  #unknown(operation: HelperOperation | 'prepare', commandId?: string, stopped = false): RemoteFsError {
    this.#retained = true;
    return typedFsError('FS_IO_ERROR', { operation, sandboxId: this.#sandboxId, ...(commandId ? { commandId } : {}), outcome: 'unknown', remoteState: stopped ? 'stopped' : 'unknown' });
  }
  async #io<T>(work: Promise<T>, operation: HelperOperation | 'prepare'): Promise<T> {
    let timer: ReturnType<typeof setTimeout> | undefined;
    try { return await Promise.race([work, new Promise<never>((_, reject) => { timer = setTimeout(() => reject(this.#unknown(operation)), this.#timeout); })]); }
    catch (error) { if (error instanceof RemoteFsError) throw error; throw this.#unknown(operation); }
    finally { clearTimeout(timer); }
  }
  async #run(argv: string[], operation: HelperOperation | 'prepare', signal?: AbortSignal): Promise<HelperCommandResult> {
    abortCheck(signal); this.#assertOpen();
    const streamAbort = new AbortController(); let commandId: string | undefined; let stdout = ''; let stdoutEvents = 0;
    const iterator = this.#transport.runStream(argv, { workingDirectory: '/', background: false, timeoutSeconds: Math.ceil(this.#timeout / 1000) }, streamAbort.signal)[Symbol.asyncIterator]();
    let terminalRelease: Promise<void> | undefined;
    const releaseTerminal = async () => {
      let timer: ReturnType<typeof setTimeout> | undefined;
      try {
        // SDK abort stops applying after headers; returning its iterator cancels the body reader.
        await Promise.race([Promise.resolve().then(() => iterator.return?.()), new Promise<void>(resolve => {
          timer = setTimeout(resolve, this.#settlement);
        })]);
      } catch { /* Stream cleanup cannot replace a known filesystem outcome. */ }
      finally { clearTimeout(timer); }
    };
    let settled = false; let stopping = false; let interrupted = false; let polling = false;
    let deadline: ReturnType<typeof setTimeout> | undefined; let stopTimer: ReturnType<typeof setTimeout> | undefined;
    let resolve!: (value: HelperCommandResult) => void; let reject!: (error: RemoteFsError) => void;
    const done = new Promise<HelperCommandResult>((yes, no) => { resolve = yes; reject = no; });
    const clear = () => { clearTimeout(deadline); clearTimeout(stopTimer); signal?.removeEventListener('abort', onAbort); };
    const finish = (exitCode: number | null) => {
      if (settled) return; settled = true; clear(); streamAbort.abort();
      resolve({ stdout, stdoutEvents, exitCode, ...(commandId ? { commandId } : {}) });
    };
    const unknown = () => { if (settled) return; settled = true; clear(); streamAbort.abort(); reject(this.#unknown(operation, commandId)); };
    const interrupt = () => {
      if (!commandId || interrupted || this.#binding.state !== 'open') return;
      interrupted = true; void this.#transport.interrupt(commandId).catch(() => undefined);
    };
    const poll = async () => {
      if (!commandId || polling || settled) return; polling = true;
      const end = Date.now() + this.#settlement;
      try {
        while (!settled && Date.now() < end) {
          let status;
          let timeout: ReturnType<typeof setTimeout> | undefined;
          try {
            status = await Promise.race([this.#transport.getCommandStatus(commandId), new Promise<undefined>(resolve => {
              timeout = setTimeout(() => resolve(undefined), Math.max(1, end - Date.now()));
            })]);
          } catch { status = undefined; } finally { clearTimeout(timeout); }
          if (status && (status.id === undefined || status.id === commandId) && status.running === false) {
            finish(typeof status.exitCode === 'number' && Number.isSafeInteger(status.exitCode) ? status.exitCode : null); return;
          }
          if (!settled) await new Promise<void>(resolve => setTimeout(resolve, Math.min(20, Math.max(1, end - Date.now()))));
        }
      } finally { polling = false; }
    };
    const stop = (cancel: boolean) => {
      if (settled) return; if (!stopping) {
        stopping = true; clearTimeout(deadline); streamAbort.abort(); stopTimer = setTimeout(unknown, this.#settlement);
      }
      if (cancel) interrupt(); void poll();
    };
    const onAbort = () => stop(true);
    signal?.addEventListener('abort', onAbort, { once: true });
    deadline = setTimeout(() => stop(true), this.#timeout);
    if (signal?.aborted) onAbort();
    // Keep observing late init: an abort that precedes the ID still causes an explicit interrupt.
    void (async () => {
      try {
        for (;;) {
          const item = await iterator.next(); if (item.done) break; const event = item.value;
          if (event.type === 'init' && !commandId && typeof event.text === 'string' && /^[A-Za-z0-9._:-]{1,256}$/.test(event.text)) {
            commandId = event.text; if (stopping) { interrupt(); void poll(); }
          } else if (!settled && event.type === 'stdout') {
            const text = event.text ?? ''; if (typeof text !== 'string' || Buffer.byteLength(stdout, 'utf8') + Buffer.byteLength(text, 'utf8') > 8192) { unknown(); continue; }
            stdout += text; stdoutEvents = Math.min(2, stdoutEvents + 1);
          } else if (event.type === 'execution_complete') {
            terminalRelease = releaseTerminal(); finish(0); return;
          } else if (event.type === 'error') {
            const name = event.error?.ename ?? event.error?.name; const value = event.error?.evalue ?? event.error?.value;
            const code = name === 'CommandExecError' && typeof value === 'string' && /^-?\d+$/.test(value) ? Number(value) : null;
            terminalRelease = releaseTerminal(); finish(code !== null && Number.isSafeInteger(code) ? code : null); return;
          }
        }
        if (!settled) { if (commandId) stop(false); else unknown(); }
      } catch { if (!settled) { if (commandId) stop(false); else unknown(); } }
    })();
    const result = await done; await terminalRelease; return result;
  }
  ready = async (signal?: AbortSignal): Promise<void> => {
    this.#assertOpen(); abortCheck(signal);
    // Shared preparation belongs to the provider. Individual callers cancel only their own wait.
    this.#ready ??= this.#prepare();
    const combined = signal ? AbortSignal.any([signal, this.#shutdown.signal]) : this.#shutdown.signal;
    let onAbort!: () => void;
    try {
      await Promise.race([this.#ready, new Promise<never>((_, reject) => {
        onAbort = () => reject(typedFsError('FS_ABORTED'));
        combined.addEventListener('abort', onAbort, { once: true }); if (combined.aborted) onAbort();
      })]);
    } finally { combined.removeEventListener('abort', onAbort); }
    abortCheck(combined); this.#assertOpen();
  };
  async #prepare(): Promise<void> {
    const combined = this.#shutdown.signal;
    abortCheck(combined);
    // Asset ruling: this exact static packaged code path is the only host read.
    const bytes = await readFile(new URL('../remote/fs_helper.py', import.meta.url)).catch(() => fail());
    if (bytes.length > MAX_DATA_BYTES) fail('FS_TOO_LARGE'); abortCheck(combined);
    const created = await this.#run(['python3', '-c', BOOTSTRAP], 'prepare', combined);
    const directory = singleStdoutRecord(created);
    if (!directory || !/^\/tmp\/opensandbox-dsh-[A-Za-z0-9_-]+$/.test(directory)) throw this.#unknown('prepare', created.commandId, true);
    this.#directory = directory; abortCheck(combined);
    const path = `${this.#directory}/fs_helper.py`;
    // SDK/execd expect octal-digit numbers (600), not JavaScript POSIX-bit literals (0o600).
    await this.#io(this.#transport.writeBytes(path, bytes, { mode: 600 }), 'prepare'); abortCheck(combined);
    const hash = createHash('sha256').update(bytes).digest('hex');
    const checked = await this.#run(['python3', '-c', VERIFY_HELPER, path, hash, String(bytes.length)], 'prepare', combined);
    if (checked.exitCode !== 0) fail(); abortCheck(combined);
    // No fake ping: check the actual versioned resolve/stat protocol against the physical cwd.
    const target = await this.#send('resolve', { path: '.', cwd: this.#cwd }, combined);
    const info = await this.#send('stat', { target }, combined);
    if (info?.type !== 'directory') fail('FS_NOT_DIRECTORY');
  }
  request = async <O extends HelperOperation>(operation: O, args: HelperArgs[O], signal?: AbortSignal): Promise<HelperResults[O]> => {
    this.#assertOpen(); abortCheck(signal); validateArgs(operation, args);
    // Snapshot validated JSON data so a caller cannot mutate it across readiness awaits.
    const bytes = Buffer.from(JSON.stringify({ protocol: 1, operation, args }), 'utf8');
    if (bytes.length > MAX_REQUEST_BYTES) fail('FS_TOO_LARGE');
    const snapshot = JSON.parse(bytes.toString('utf8')).args as HelperArgs[O];
    const combined = signal ? AbortSignal.any([signal, this.#shutdown.signal]) : this.#shutdown.signal;
    const work = (async () => { await this.ready(combined); return this.#send(operation, snapshot, combined); })();
    this.#active.add(work); void work.then(() => this.#active.delete(work), () => this.#active.delete(work)); return work;
  };
  async #send<O extends HelperOperation>(operation: O, args: HelperArgs[O], signal?: AbortSignal): Promise<HelperResults[O]> {
    this.#assertOpen(); abortCheck(signal);
    const directory = this.#directory!; let requestDirectory: string | undefined; let launched = false; let known = false;
    let commandId: string | undefined; let stopped = false;
    try {
      const made = await this.#run(['python3', '-c', REQUEST_DIRECTORY, directory], operation, signal);
      const record = singleStdoutRecord(made);
      if (!record || !record.startsWith(`${directory}/request-`) || !/^request-[A-Za-z0-9_-]+$/.test(record.slice(directory.length + 1))) throw this.#unknown(operation, made.commandId, true);
      requestDirectory = record; abortCheck(signal);
      const input = `${requestDirectory}/request.json`, output = `${requestDirectory}/response.json`;
      const bytes = Buffer.from(JSON.stringify({ protocol: 1, operation, args }), 'utf8');
      if (bytes.length > MAX_REQUEST_BYTES) fail('FS_TOO_LARGE');
      await this.#io(this.#transport.writeBytes(input, bytes, { mode: 600 }), operation); abortCheck(signal); this.#assertOpen();
      launched = true;
      const result = await this.#run(['python3', `${directory}/fs_helper.py`, input, output], operation, signal);
      commandId = result.commandId; stopped = true;
      if (result.exitCode !== 0) throw this.#unknown(operation, commandId, true);
      // Process completion is necessary. Empty/partial/lost responses never establish a commit outcome.
      const responseBytes = await this.#io(this.#download(output), operation);
      let decoded: string; try { decoded = new TextDecoder('utf-8', { fatal: true }).decode(responseBytes); } catch { fail(); }
      const envelope = parseResponse(decoded);
      assertObject(envelope, ['protocol', 'ok'], ['result', 'error']); if (envelope.protocol !== 1 || typeof envelope.ok !== 'boolean') fail();
      if (envelope.ok) {
        assertObject(envelope, ['protocol', 'ok', 'result']); validateResult(operation, envelope.result, args); known = true;
        return envelope.result as HelperResults[O];
      }
      assertObject(envelope, ['protocol', 'ok', 'error']); assertObject(envelope.error, ['code', 'message']);
      assertString(envelope.error.message, MAX_PATH_BYTES);
      if (typeof envelope.error.code !== 'string' || !Object.hasOwn(MESSAGES, envelope.error.code)) fail();
      known = true;
      throw typedFsError(envelope.error.code as FsErrorCode, { operation, sandboxId: this.#sandboxId, ...(commandId ? { commandId } : {}), remoteState: 'stopped' });
    } catch (error) {
      if (launched && !known) {
        this.#retained = true;
        if (error instanceof RemoteFsError && error.outcome === 'unknown') {
          // #run can reject before the successful-return assignment captures its known ID.
          throw this.#unknown(operation, error.commandId ?? commandId, stopped || error.remoteState === 'stopped');
        }
        if (operation === 'write-text' || operation === 'edit-text' || !(error instanceof FsError)) throw this.#unknown(operation, commandId, stopped);
      }
      if (error instanceof FsError) throw error; throw typedFsError('FS_IO_ERROR', { operation, sandboxId: this.#sandboxId });
    } finally {
      if (requestDirectory && ((!launched && !this.#retained) || known) && this.#binding.state === 'open') {
        await this.#io(this.#transport.deleteDirectories([requestDirectory]), operation).catch(() => { this.#retained = true; });
      }
    }
  }
  async #download(path: string): Promise<Uint8Array> {
    // Keep memory byte-bounded even when the SDK produces millions of tiny chunks.
    let buffer = new Uint8Array(8192); let total = 0;
    for await (const chunk of this.#transport.readBytesStream(path, { limit: MAX_RESPONSE_BYTES + 1 })) {
      if (!(chunk instanceof Uint8Array)) fail(); total += chunk.byteLength;
      if (total > MAX_RESPONSE_BYTES) fail('FS_TOO_LARGE');
      if (total > buffer.length) {
        const expanded = new Uint8Array(Math.min(MAX_RESPONSE_BYTES, Math.max(total, buffer.length * 2)));
        expanded.set(buffer); buffer = expanded;
      }
      buffer.set(chunk, total - chunk.byteLength);
    }
    return buffer.slice(0, total);
  }
  dispose = (): Promise<void> => {
    this.#disposing ??= (async () => {
      this.#disposed = true; this.#shutdown.abort();
      await Promise.allSettled([...this.#active, ...(this.#ready ? [this.#ready] : [])]);
      if (this.#directory && !this.#retained && this.#binding.state === 'open') {
        await this.#io(this.#transport.deleteDirectories([this.#directory]), 'prepare').catch(() => { this.#retained = true; });
      }
    })(); return this.#disposing;
  };
}
