// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createBindingOpener } from '../src/binding.js';
import type { OpenOptions } from '../src/types.js';
import { deferred, execution, fakeSandbox, fakeSdk } from './fixtures/sdk-facade.js';

const openingModes = ['create-image', 'create-template', 'connect'] as const;
const options = (kind: OpenOptions['kind'], overrides: Partial<OpenOptions> = {}): OpenOptions => ({
  kind, image: 'python:3.11', templateId: 'template-a', timeoutSeconds: 300,
  sandboxId: 'sandbox-a', sessionId: 'session-a', remoteCwd: '/workspace', connectionConfig: {},
  ...overrides,
} as OpenOptions);

// Execute the emitted Python program unchanged, with only version_info simulated.
function checkPythonVersion(command: string | string[], cwd: string, major: number, minor: number, optimize = '0') {
  if (!Array.isArray(command)) throw new Error('Expected fixed prerequisite argv');
  const script = command[2]?.match(/python3 -c '([^']+)'/)?.[1];
  if (!script) throw new Error('Expected emitted Python prerequisite program');
  return spawnSync('python3', ['-c', [
    'import os,sys,json,hashlib,fcntl,tempfile,stat,collections',
    'sys.version_info=collections.namedtuple("version_info","major minor micro releaselevel serial")(int(sys.argv[2]),int(sys.argv[3]),0,"final",0)',
    'exec(sys.argv[1])',
  ].join('; '), script, String(major), String(minor)], {
    cwd, env: { PATH: process.env.PATH, PYTHONOPTIMIZE: optimize }, encoding: 'utf8', timeout: 5_000,
  });
}

afterEach(() => { vi.restoreAllMocks(); vi.useRealTimers(); });

describe('binding Python prerequisites', () => {
  it.each(['0', '1', '2'])('enforces Python >=3.8 with PYTHONOPTIMIZE=%s', async optimize => {
    const fake = fakeSandbox();
    const binding = await createBindingOpener(fakeSdk(fake.sandbox))(options('create-image'));
    const cwd = mkdtempSync(join(tmpdir(), 'dsh-preflight-version-'));
    try {
      const command = fake.commands.run.mock.calls[0]![0];
      for (const [major, minor, accepted] of [[2, 7, false], [3, 4, false], [3, 7, false], [3, 8, true], [3, 12, true], [4, 0, true]] as const) {
        const check = checkPythonVersion(command, cwd, major, minor, optimize);
        expect(check.error).toBeUndefined();
        expect(check.status, `Python ${major}.${minor}`).toBe(accepted ? 0 : 1);
        expect(check.stderr).toBe('');
      }
    } finally { await binding.close(); rmSync(cwd, { recursive: true }); }
  });

  it.each(openingModes)('rejects Python 3.7 and releases %s according to ownership', async kind => {
    const fake = fakeSandbox();
    const cwd = mkdtempSync(join(tmpdir(), 'dsh-preflight-version-'));
    fake.commands.run.mockImplementation(async command => {
      const check = checkPythonVersion(command, cwd, 3, 7);
      expect(check.error).toBeUndefined();
      return execution(check.status ?? 1);
    });
    try {
      await expect(createBindingOpener(fakeSdk(fake.sandbox))(options(kind, { remoteCwd: cwd }))).rejects.toMatchObject({
        code: 'binding_open_failed', stage: 'preflight', cleanupState: kind === 'connect' ? 'not-owned' : 'killed',
      });
      expect(fake.boundary.kill).toHaveBeenCalledTimes(kind === 'connect' ? 0 : 1);
      expect(fake.boundary.close).toHaveBeenCalledOnce();
    } finally { rmSync(cwd, { recursive: true }); }
  });
});

describe('binding preflight timer range', () => {
  it.each(openingModes)('rejects unrepresentable waits before any SDK call for %s', async kind => {
    for (const timeout of [(2_147_483_647 + 0.1) / 1000, 2_147_483.648, Number.MAX_VALUE, Infinity]) {
      const fake = fakeSandbox();
      const sdk = fakeSdk(fake.sandbox);
      await expect(createBindingOpener(sdk)(options(kind, { preflightTimeoutSeconds: timeout }))).rejects.toMatchObject({ code: 'invalid_options' });
      expect(sdk.create).not.toHaveBeenCalled();
      expect(sdk.createFromTemplate).not.toHaveBeenCalled();
      expect(sdk.connect).not.toHaveBeenCalled();
      expect(fake.boundary.waitUntilReady).not.toHaveBeenCalled();
      expect(fake.commands.run).not.toHaveBeenCalled();
      expect(fake.boundary.kill).not.toHaveBeenCalled();
      expect(fake.boundary.close).not.toHaveBeenCalled();
    }
  });

  it.each([
    [2_147_483.647, 2_147_483_647],
    [(2_147_483_647 - 0.1) / 1000, 2_147_483_647],
    [0.0001, 1],
  ])('accepts %s seconds and rounds the local timer to %s ms', async (seconds, milliseconds) => {
    vi.useFakeTimers();
    const timer = vi.spyOn(globalThis, 'setTimeout');
    const fake = fakeSandbox();
    const started = deferred<void>();
    const result = deferred<ReturnType<typeof execution>>();
    fake.commands.run.mockImplementation(async () => { started.resolve(); return result.promise; });
    const pending = createBindingOpener(fakeSdk(fake.sandbox))(options('create-image', { preflightTimeoutSeconds: seconds! }));
    await started.promise;
    expect(timer).toHaveBeenCalledWith(expect.any(Function), milliseconds);
    expect(fake.commands.run.mock.calls[0]?.[1]?.timeoutSeconds).toBe(seconds);
    result.resolve(execution());
    const binding = await pending;
    expect(vi.getTimerCount()).toBe(0);
    await binding.close();
  });

  it('keeps the rounded deadline and interrupts a command whose SDK ignores abort', async () => {
    vi.useFakeTimers();
    const fake = fakeSandbox();
    const started = deferred<void>();
    fake.commands.run.mockImplementation(async (_command, _options, handlers) => {
      await handlers?.onInit?.({ id: 'preflight-command', timestamp: 1 });
      started.resolve();
      return new Promise(() => {});
    });
    const pending = createBindingOpener(fakeSdk(fake.sandbox))(options('create-image', { preflightTimeoutSeconds: 1.0001 }));
    const rejected = expect(pending).rejects.toMatchObject({ stage: 'preflight', cleanupState: 'killed' });
    await started.promise;
    await vi.advanceTimersByTimeAsync(1000);
    expect(fake.commands.interrupt).not.toHaveBeenCalled();
    expect(fake.boundary.close).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    await rejected;
    expect(fake.commands.interrupt).toHaveBeenCalledExactlyOnceWith('preflight-command');
    expect(fake.boundary.kill).toHaveBeenCalledOnce();
    expect(fake.boundary.close).toHaveBeenCalledOnce();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('cancels an attached preflight at the maximum wait without deleting the sandbox', async () => {
    vi.useFakeTimers();
    const fake = fakeSandbox();
    const started = deferred<void>();
    const abort = new AbortController();
    fake.commands.run.mockImplementation(async (_command, _options, handlers) => {
      await handlers?.onInit?.({ id: 'preflight-command', timestamp: 1 });
      started.resolve();
      return new Promise(() => {});
    });
    const pending = createBindingOpener(fakeSdk(fake.sandbox))(options('connect', {
      preflightTimeoutSeconds: 2_147_483.647, signal: abort.signal,
    }));
    const rejected = expect(pending).rejects.toMatchObject({ stage: 'preflight', cleanupState: 'not-owned' });
    await started.promise;
    abort.abort(new Error('private cancellation reason'));
    await rejected;
    expect(fake.commands.interrupt).toHaveBeenCalledExactlyOnceWith('preflight-command');
    expect(fake.boundary.kill).not.toHaveBeenCalled();
    expect(fake.boundary.close).toHaveBeenCalledOnce();
    expect(vi.getTimerCount()).toBe(0);
  });
});
