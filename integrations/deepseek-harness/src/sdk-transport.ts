// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type {
  CommandExecution, CommandStatus, ExecutionHandlers, RunCommandOpts,
  Sandbox, SandboxFiles, ServerStreamEvent, WriteEntry,
} from '@alibaba-group/opensandbox';
import { SdkTransportError } from './errors.js';
import type { SdkTransportOperation } from './errors.js';

export type ByteReadOptions = Parameters<SandboxFiles['readBytes']>[1];
export type ByteWriteOptions = Pick<WriteEntry, 'mode' | 'owner' | 'group'>;
export type DirectoryEntry = Parameters<SandboxFiles['createDirectories']>[0][number];

/** Bound public SDK transport with pinned routing and safe promise/iterator errors. */
export class SdkTransport {
  readonly #sandbox: Sandbox;

  constructor(sandbox: Sandbox) {
    // Shallow freeze pins the SDK's actual ID and service references, not its mutable config.
    Object.freeze(sandbox);
    this.#sandbox = sandbox;
  }

  async #invoke<T>(operation: SdkTransportOperation, call: () => Promise<T>): Promise<T> {
    try { return await call(); }
    catch (failure) { throw new SdkTransportError(operation, failure); }
  }

  run(command: string | string[], options?: RunCommandOpts, handlers?: ExecutionHandlers, signal?: AbortSignal): Promise<CommandExecution> {
    return this.#invoke('run', () => this.#sandbox.commands.run(command, options, handlers, signal));
  }

  async *runStream(command: string | string[], options?: RunCommandOpts, signal?: AbortSignal): AsyncIterable<ServerStreamEvent> {
    try { yield* this.#sandbox.commands.runStream(command, options, signal); }
    catch (failure) { throw new SdkTransportError('runStream', failure); }
  }

  interrupt(commandId: string): Promise<void> {
    return this.#invoke('interrupt', () => this.#sandbox.commands.interrupt(commandId));
  }

  getCommandStatus(commandId: string): Promise<CommandStatus> {
    return this.#invoke('getCommandStatus', () => this.#sandbox.commands.getCommandStatus(commandId));
  }

  writeBytes(path: string, bytes: Uint8Array, options?: ByteWriteOptions): Promise<void> {
    return this.#invoke('writeBytes', () => this.#sandbox.files.writeFiles([{ path, data: bytes, ...options }]));
  }

  readBytes(path: string, options?: ByteReadOptions): Promise<Uint8Array> {
    return this.#invoke('readBytes', () => this.#sandbox.files.readBytes(path, options));
  }

  async *readBytesStream(path: string, options?: ByteReadOptions): AsyncIterable<Uint8Array> {
    try { yield* this.#sandbox.files.readBytesStream(path, options); }
    catch (failure) { throw new SdkTransportError('readBytesStream', failure); }
  }

  createDirectories(entries: DirectoryEntry[]): Promise<void> {
    return this.#invoke('createDirectories', () => this.#sandbox.files.createDirectories(entries));
  }

  deleteFiles(paths: string[]): Promise<void> {
    return this.#invoke('deleteFiles', () => this.#sandbox.files.deleteFiles(paths));
  }

  deleteDirectories(paths: string[]): Promise<void> {
    return this.#invoke('deleteDirectories', () => this.#sandbox.files.deleteDirectories(paths));
  }
}
