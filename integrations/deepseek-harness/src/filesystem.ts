// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { createHash } from 'node:crypto';
import { posix } from 'node:path';
import type { Context } from '@deepseek-ai/cordis';
import { FileSystem, FsTargetKey, FsVersion } from '@deepseek-ai/dsh-fs';
import type { FsDirEntry, FsEditOutcome, FsEditRequest, FsInfo, FsPathInfo, FsTarget, FsWriteIntent, FsWriteOutcome } from '@deepseek-ai/dsh-fs';
import {
  HelperClient, MAX_DATA_BYTES, assertInteger, assertObject, assertString, decodeBase64, typedFsError,
} from './helper-client.js';
import type { HelperClientOptions, HelperTarget } from './helper-client.js';
import type { BoundSandbox } from './types.js';

export interface RemoteFileSystemConfig extends HelperClientOptions { binding: BoundSandbox; }
const STREAM_CHUNK_BYTES = 64 * 1024;
type Policy = Parameters<FileSystem['writeText']>[4];

/** One remote execution world. No ambient actor/session routing and no host path mapping. */
export class RemoteFileSystem extends FileSystem {
  readonly #helper: HelperClient; readonly #cwd: string; readonly #scope: readonly string[];
  readonly #targets = new WeakMap<object, Readonly<HelperTarget>>();
  #disposed = false;

  constructor(ctx: Context, config: RemoteFileSystemConfig) {
    super(ctx); this.#helper = new HelperClient(config.binding, config);
    const { sandboxId, sessionId } = this.#helper.identity;
    this.#scope = Object.freeze([sandboxId, sessionId]); this.#cwd = config.binding.descriptor.remoteCwd;
    ctx.effect(() => () => this.dispose(), 'remote filesystem ownership');
  }
  #open(): void { if (this.#disposed) throw typedFsError('FS_IO_ERROR'); }
  #target(remote: HelperTarget): FsTarget {
    const key = createHash('sha256').update(JSON.stringify([...this.#scope, remote.path])).digest('hex');
    const target = Object.freeze({ targetKey: FsTargetKey(`opensandbox-dsh:v1:${key}`), displayPath: remote.displayPath });
    this.#targets.set(target, Object.freeze({ ...remote })); return target;
  }
  #validated(target: FsTarget): Readonly<HelperTarget> {
    this.#open(); const resolved = target && typeof target === 'object' ? this.#targets.get(target) : undefined;
    if (!resolved) throw typedFsError('FS_IO_ERROR'); return resolved;
  }
  #policy(policy: Policy): void {
    if (policy && (policy.mode !== 'danger-full-access' || (policy.sessionId !== undefined && String(policy.sessionId) !== this.#scope[1]))) {
      throw typedFsError('FS_SANDBOX_DENIED');
    }
  }
  // Cordis service proxies require lexical receivers for private routing state.
  /** Eager verified preparation for composition; every remote operation also awaits this readiness. */
  ready = async (signal?: AbortSignal): Promise<void> => { this.#open(); await this.#helper.ready(signal); };
  resolve = async (path: string, opts?: { cwd?: string; signal?: AbortSignal }): Promise<FsTarget> => {
    this.#open(); return this.#target(await this.#helper.request('resolve', { path, cwd: opts?.cwd ?? this.#cwd }, opts?.signal));
  };
  processPath = (target: FsTarget): string => this.#validated(target).path;
  processPathFromHostPath = (_hostPath: string): undefined => undefined;
  fileUrl = (target: FsTarget): string => {
    // POSIX encoding is independent of the harness host's path separator/platform.
    return 'file://' + this.processPath(target).split('/').map(segment => encodeURIComponent(segment)).join('/');
  };
  contains = (parent: FsTarget, child: FsTarget): boolean => {
    const relative = posix.relative(this.processPath(parent), this.processPath(child));
    return relative === '' || (relative !== '..' && !relative.startsWith('../') && !posix.isAbsolute(relative));
  };
  stat = async (target: FsTarget, signal?: AbortSignal): Promise<FsInfo | undefined> => {
    const info = await this.#helper.request('stat', { target: this.#validated(target) }, signal);
    return info ? { ...info, version: FsVersion(info.version) } : undefined;
  };
  lstat = async (path: string, opts?: { cwd?: string }, signal?: AbortSignal): Promise<FsPathInfo | undefined> => {
    this.#open(); const info = await this.#helper.request('lstat', { path, cwd: opts?.cwd ?? this.#cwd }, signal);
    return info ? { ...info, version: FsVersion(info.version) } : undefined;
  };
  readText = async (target: FsTarget, signal?: AbortSignal): Promise<string> => {
    return (await this.#helper.request('read-text', { target: this.#validated(target) }, signal)).text;
  };
  streamText = async (target: FsTarget, signal?: AbortSignal): Promise<AsyncIterable<string>> => {
    const remote = this.#validated(target), helper = this.#helper;
    if (signal?.aborted) throw typedFsError('FS_ABORTED');
    return (async function* () {
      const initial = await helper.request('stat', { target: remote }, signal);
      if (!initial) throw typedFsError('FS_NOT_FOUND'); if (initial.type !== 'file') throw typedFsError('FS_NOT_REGULAR_FILE');
      assertInteger(initial.size, MAX_DATA_BYTES); const size = initial.size;
      const decoder = new TextDecoder('utf-8', { fatal: true }); let offset = 0;
      // One zero-length probe also verifies empty files and the final byte boundary.
      do {
        if (signal?.aborted) throw typedFsError('FS_ABORTED');
        const length = Math.min(STREAM_CHUNK_BYTES, size - offset);
        const chunk = await helper.request('read-range', { target: remote, offset, length }, signal);
        if (chunk.version !== initial.version) throw typedFsError('FS_STALE_VERSION');
        const bytes = decodeBase64(chunk.base64, length);
        if (bytes.length !== length) throw typedFsError('FS_STALE_VERSION');
        if (bytes.includes(0)) throw typedFsError('FS_NOT_TEXT'); offset += bytes.length;
        let text: string; try { text = decoder.decode(bytes, { stream: offset < size }); } catch { throw typedFsError('FS_NOT_TEXT'); }
        if (text) yield text;
      } while (offset < size);
    })();
  };
  readBytes = async (target: FsTarget, signal: AbortSignal | undefined, maxBytes: number): Promise<Uint8Array> => {
    const result = await this.#helper.request('read-bytes', { target: this.#validated(target), maxBytes }, signal);
    return decodeBase64(result.base64, maxBytes);
  };
  readByteRange = async (target: FsTarget, range: { offset: number; length: number }, signal?: AbortSignal): Promise<Uint8Array> => {
    assertObject(range, ['offset', 'length']); assertInteger(range.offset); assertInteger(range.length, MAX_DATA_BYTES);
    const { offset, length } = range;
    if (!Number.isSafeInteger(offset + length)) throw typedFsError('FS_IO_ERROR');
    const result = await this.#helper.request('read-range', { target: this.#validated(target), offset, length }, signal);
    return decodeBase64(result.base64, length);
  };
  listDir = async (target: FsTarget, signal?: AbortSignal): Promise<FsDirEntry[]> => {
    const entries = await this.#helper.request('list', { target: this.#validated(target) }, signal);
    return entries.map(entry => ({ name: entry.name, type: entry.type, target: this.#target(entry.target),
      ...(entry.version !== undefined ? { version: FsVersion(entry.version) } : {}), ...(entry.size !== undefined ? { size: entry.size } : {}) }));
  };
  writeText = async (target: FsTarget, content: string, expected?: FsWriteIntent, signal?: AbortSignal, sandboxPolicy?: Policy): Promise<FsWriteOutcome> => {
    this.#policy(sandboxPolicy); assertString(content, MAX_DATA_BYTES, true);
    const result = await this.#helper.request('write-text', { target: this.#validated(target), content, ...(expected !== undefined ? { expected } : {}) }, signal);
    return { ...result, version: FsVersion(result.version) };
  };
  editText = async (target: FsTarget, edit: FsEditRequest, expected?: { version: FsVersion }, signal?: AbortSignal, sandboxPolicy?: Policy): Promise<FsEditOutcome> => {
    this.#policy(sandboxPolicy);
    const result = await this.#helper.request('edit-text', { target: this.#validated(target), edit, ...(expected !== undefined ? { expected } : {}) }, signal);
    return { ...result, version: FsVersion(result.version) };
  };
  dispose = async (): Promise<void> => { this.#disposed = true; await this.#helper.dispose(); };
}
