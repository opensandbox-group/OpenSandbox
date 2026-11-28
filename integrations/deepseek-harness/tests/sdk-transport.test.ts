// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { SandboxApiException, SandboxError, SandboxInternalException } from '@alibaba-group/opensandbox';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SdkTransport } from '../src/sdk-transport.js';
import { fakeSandbox } from './fixtures/sdk-facade.js';
import { offlineSdkHttp } from './fixtures/offline-sdk.js';

const token = 'fixture-Authorization-Bearer-do-not-serialize';
const apiFailure = () => new SandboxApiException({
  message: token, statusCode: 403, rawBody: { Authorization: token }, cause: new Error(token),
  error: new SandboxError('UNEXPECTED_RESPONSE', token), requestId: token,
});

function assertSafe(error: unknown, operation: string, classification = 'api') {
  expect(error).toMatchObject({ name: 'SdkTransportError', code: 'sdk_transport_failed', operation, classification });
  expect(JSON.stringify(error)).not.toContain(token);
  expect(String(error)).not.toContain(token);
  expect(error).not.toHaveProperty('rawBody');
  expect(error).not.toHaveProperty('cause');
  expect(error).not.toHaveProperty('headers');
  expect(error).not.toHaveProperty('requestId');
}

afterEach(() => vi.unstubAllGlobals());

describe('safe SDK transport failures', () => {
  it.each(['run', 'interrupt', 'getCommandStatus', 'writeBytes', 'readBytes', 'createDirectories', 'deleteFiles', 'deleteDirectories'] as const)(
    'sanitizes %s promise failures', async method => {
      const fake = fakeSandbox();
      const transport = new SdkTransport(fake.sandbox);
      const failure = apiFailure();
      fake.commands.run.mockRejectedValue(failure);
      fake.commands.interrupt.mockRejectedValue(failure);
      fake.commands.getCommandStatus.mockRejectedValue(failure);
      fake.files.writeFiles.mockRejectedValue(failure);
      fake.files.readBytes.mockRejectedValue(failure);
      fake.files.createDirectories.mockRejectedValue(failure);
      fake.files.deleteFiles.mockRejectedValue(failure);
      fake.files.deleteDirectories.mockRejectedValue(failure);
      const calls = {
        run: () => transport.run(['bash', '-c', 'true']), interrupt: () => transport.interrupt('command'),
        getCommandStatus: () => transport.getCommandStatus('command'), writeBytes: () => transport.writeBytes('/data', new Uint8Array()),
        readBytes: () => transport.readBytes('/data'), createDirectories: () => transport.createDirectories([{ path: '/data' }]),
        deleteFiles: () => transport.deleteFiles(['/data']), deleteDirectories: () => transport.deleteDirectories(['/data']),
      };
      const error: unknown = await calls[method]().catch(error => error);
      assertSafe(error, method);
      expect(error).toMatchObject({ statusCode: 403, sdkCode: 'UNEXPECTED_RESPONSE' });
    },
  );

  it.each(['runStream', 'readBytesStream'] as const)('sanitizes %s failures after a chunk was delivered', async method => {
    const fake = fakeSandbox();
    fake.commands.runStream.mockImplementation(async function* () {
      yield { type: 'init', text: 'command' };
      throw apiFailure();
    });
    fake.files.readBytesStream.mockImplementation(async function* () {
      yield new Uint8Array([0, 255]);
      throw apiFailure();
    });
    const transport = new SdkTransport(fake.sandbox);
    const iterator = method === 'runStream' ? transport.runStream(['bash', '-c', 'true']) : transport.readBytesStream('/data');
    const delivered: unknown[] = [];
    let error: unknown;
    try { for await (const chunk of iterator) delivered.push(chunk); } catch (failure) { error = failure; }
    expect(delivered).toHaveLength(1);
    assertSafe(error, method);
    expect(error).toMatchObject({ statusCode: 403 });
  });

  it('omits arbitrary SDK error codes and invalid status classifications', async () => {
    const fake = fakeSandbox();
    fake.files.readBytes.mockRejectedValue(new SandboxApiException({
      message: token, statusCode: Number.NaN, error: new SandboxError(token, token), rawBody: token,
    }));
    const error: unknown = await new SdkTransport(fake.sandbox).readBytes('/data').catch(error => error);
    assertSafe(error, 'readBytes');
    expect(error).not.toHaveProperty('sdkCode');
    expect(error).not.toHaveProperty('statusCode');
  });

  it.each([
    { failure: new DOMException(token, 'AbortError'), classification: 'aborted' },
    { failure: new SandboxInternalException({ message: token, cause: new Error(token) }), classification: 'transport' },
  ])('retains the safe $classification classification', async ({ failure, classification }) => {
    const fake = fakeSandbox();
    fake.commands.run.mockRejectedValue(failure);
    const error: unknown = await new SdkTransport(fake.sandbox).run(['true']).catch(error => error);
    assertSafe(error, 'run', classification);
  });

  it.each(['readBytes', 'readBytesStream', 'runStream'] as const)('sanitizes real SDK default-adapter %s failures', async method => {
    const http = offlineSdkHttp();
    const sandbox = await http.connect('errors');
    http.fail(403, token);
    const transport = new SdkTransport(sandbox);
    let error: unknown;
    try {
      if (method === 'readBytes') await transport.readBytes('/data');
      else {
        const iterator = method === 'runStream' ? transport.runStream(['true']) : transport.readBytesStream('/data');
        for await (const _ of iterator) { /* Observe the full real SDK iterator. */ }
      }
    } catch (failure) { error = failure; }
    finally { await sandbox.close(); }
    assertSafe(error, method);
    expect(error).toMatchObject({ statusCode: 403, sdkCode: 'UNEXPECTED_RESPONSE' });
  });
});
