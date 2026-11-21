// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import type {
  CommandExecution, CommandStatus, ExecdCommands, Sandbox, SandboxFiles,
  ServerStreamEvent,
} from '@alibaba-group/opensandbox';
import { vi } from 'vitest';
import type { SdkFacade } from '../../src/types.js';

export function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

export function execution(exitCode = 0): CommandExecution {
  return {
    id: 'preflight-command', logs: { stdout: [], stderr: [] }, result: [],
    complete: { timestamp: 1, executionTimeMs: 1 }, exitCode,
  };
}

/** Explicit SDK test boundary; it never sends requests or executes host commands. */
export function fakeSandbox(id = 'sandbox-a') {
  let transportClosed = false;
  const commands = {
    run: vi.fn<ExecdCommands['run']>().mockResolvedValue(execution()),
    runStream: vi.fn<ExecdCommands['runStream']>().mockImplementation(async function* () {
      yield { type: 'init', text: 'command-a' } satisfies ServerStreamEvent;
      yield { type: 'stdout', text: 'hello' } satisfies ServerStreamEvent;
      yield { type: 'stderr', text: 'warning' } satisfies ServerStreamEvent;
      yield { type: 'execution_complete' } satisfies ServerStreamEvent;
    }),
    interrupt: vi.fn<ExecdCommands['interrupt']>().mockResolvedValue(undefined),
    getCommandStatus: vi.fn<ExecdCommands['getCommandStatus']>().mockResolvedValue({
      id: 'command-a', running: false, exitCode: 0,
    } satisfies CommandStatus),
  };
  const files = {
    writeFiles: vi.fn<SandboxFiles['writeFiles']>().mockResolvedValue(undefined),
    readBytes: vi.fn<SandboxFiles['readBytes']>().mockResolvedValue(new Uint8Array([0, 255, 128, 10])),
    readBytesStream: vi.fn<SandboxFiles['readBytesStream']>().mockImplementation(async function* () {
      yield new Uint8Array([0, 255]);
      yield new Uint8Array([128, 10]);
    }),
    createDirectories: vi.fn<SandboxFiles['createDirectories']>().mockResolvedValue(undefined),
    deleteFiles: vi.fn<SandboxFiles['deleteFiles']>().mockResolvedValue(undefined),
    deleteDirectories: vi.fn<SandboxFiles['deleteDirectories']>().mockResolvedValue(undefined),
  };
  const boundary = {
    id, commands, files,
    // Deliberately include private connection data to catch accidental serialization.
    connectionConfig: { apiKey: 'do-not-serialize', headers: { Authorization: 'Bearer secret' } },
    waitUntilReady: vi.fn<Sandbox['waitUntilReady']>().mockResolvedValue(undefined),
    close: vi.fn<Sandbox['close']>().mockImplementation(async () => { transportClosed = true; }),
    kill: vi.fn<Sandbox['kill']>().mockImplementation(async () => {
      if (transportClosed) throw new Error('offline fixture: closed dispatcher');
    }),
  };
  // The fixture implements only the explicit facade surface used by this package.
  return { sandbox: boundary as unknown as Sandbox, boundary, commands, files };
}

export function fakeSdk(sandbox: Sandbox) {
  return {
    create: vi.fn<SdkFacade['create']>().mockResolvedValue(sandbox),
    createFromTemplate: vi.fn<SdkFacade['createFromTemplate']>().mockResolvedValue(sandbox),
    connect: vi.fn<SdkFacade['connect']>().mockResolvedValue(sandbox),
  } satisfies SdkFacade;
}
