// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { inspect } from 'node:util';
import { InvalidArgumentException, SandboxApiException, SandboxError } from '@alibaba-group/opensandbox';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { createBindingOpener, openBinding } from '../src/binding.js';
import { UnknownOutcomeError } from '../src/errors.js';
import type { OpenOptions } from '../src/types.js';
import { fakeSandbox, fakeSdk } from './fixtures/sdk-facade.js';

const secret = 'fixture-private-header-body-message-request-id';
const options = (kind: 'create-image' | 'create-template'): OpenOptions => ({
  ...(kind === 'create-image' ? { kind, image: 'python:3.11' } : { kind, templateId: 'template-a', timeoutSeconds: 300 }),
  sessionId: 's', remoteCwd: '/workspace', connectionConfig: { domain: 'offline.invalid', disableMetrics: true },
});
const apiFailure = (statusCode = 429, code = 'FSB::API_ERROR') => new SandboxApiException({
  message: secret, statusCode, error: new SandboxError(code, secret),
  rawBody: { message: secret, headers: { Authorization: secret } }, requestId: secret, cause: new Error(secret),
});
function assertSafe(error: unknown) {
  expect(JSON.stringify(error)).not.toContain(secret);
  expect(String(error)).not.toContain(secret);
  expect(inspect(error, { showHidden: true, depth: 10 })).not.toContain(secret);
  for (const key of ['cause', 'rawBody', 'headers', 'requestId', 'error']) expect(error).not.toHaveProperty(key);
}
afterEach(() => vi.unstubAllGlobals());

describe('safe SDK opening diagnostics', () => {
  it.each(['create-image', 'create-template'] as const)('retains an API status and allowlisted code for %s without retrying', async kind => {
    const fake = fakeSandbox(); const sdk = fakeSdk(fake.sandbox);
    const create = kind === 'create-image' ? sdk.create : sdk.createFromTemplate;
    create.mockRejectedValue(apiFailure());
    const error = await createBindingOpener(sdk)(options(kind)).catch(error => error);
    expect(error).toBeInstanceOf(UnknownOutcomeError);
    expect(error).toMatchObject({ code: 'outcome_unknown', operation: 'create', classification: 'api', statusCode: 429, sdkCode: 'FSB::API_ERROR' });
    expect(error.correlationId).toMatch(/^[0-9a-f-]{36}$/);
    expect(create).toHaveBeenCalledOnce(); expect(fake.boundary.kill).not.toHaveBeenCalled();
    assertSafe(error);
  });

  it('retains typed SDK validation facts without copying the rejected argument', async () => {
    const fake = fakeSandbox(); const sdk = fakeSdk(fake.sandbox);
    sdk.create.mockRejectedValue(new InvalidArgumentException({ message: secret, cause: new Error(secret) }));
    const error = await createBindingOpener(sdk)(options('create-image')).catch(error => error);
    expect(error).toMatchObject({ code: 'outcome_unknown', classification: 'invalid-argument', sdkCode: 'INVALID_ARGUMENT' });
    expect(error).not.toHaveProperty('statusCode'); expect(sdk.create).toHaveBeenCalledOnce(); assertSafe(error);
  });

  it.each([NaN, Infinity, 99, 600, 429.5])('omits invalid HTTP status %s and arbitrary error codes', async status => {
    const fake = fakeSandbox(); const sdk = fakeSdk(fake.sandbox);
    sdk.create.mockRejectedValue(apiFailure(status, secret));
    const error = await createBindingOpener(sdk)(options('create-image')).catch(error => error);
    expect(error).toMatchObject({ classification: 'api' });
    expect(error).not.toHaveProperty('statusCode'); expect(error).not.toHaveProperty('sdkCode'); assertSafe(error);
  });

  it.each([
    { failure: new Error(secret), classification: 'transport' },
    { failure: new DOMException(secret, 'AbortError'), classification: 'aborted' },
    { failure: { name: 'SandboxApiException', statusCode: 429, error: { code: secret } }, classification: 'transport' },
  ])('keeps $classification failures unknown and never infers rejection from error messages', async ({ failure, classification }) => {
    const fake = fakeSandbox(); const sdk = fakeSdk(fake.sandbox); sdk.create.mockRejectedValue(failure);
    const error = await createBindingOpener(sdk)(options('create-image')).catch(error => error);
    expect(error).toMatchObject({ code: 'outcome_unknown', classification });
    expect(error).not.toHaveProperty('statusCode'); expect(sdk.create).toHaveBeenCalledOnce(); assertSafe(error);
  });

  it.each(['create-image', 'create-template'] as const)('preserves real SDK HTTP 429 rejection facts for %s', async kind => {
    const fetch = vi.fn<typeof globalThis.fetch>().mockResolvedValue(Response.json({ code: 'FSB::API_ERROR', message: secret },
      { status: 429, headers: { 'x-request-id': secret } }));
    vi.stubGlobal('fetch', fetch);
    const error = await openBinding(options(kind)).catch(error => error);
    expect(error).toMatchObject({ code: 'outcome_unknown', classification: 'api', statusCode: 429, sdkCode: 'FSB::API_ERROR' });
    expect(fetch).toHaveBeenCalledOnce(); assertSafe(error);
  });

  it.each(['create-image', 'create-template'] as const)('does not mistake a post-creation HTTP failure for definite rejection in %s', async kind => {
    const requests: { path: string; method: string }[] = [];
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const request = new Request(input, init); const path = new URL(request.url).pathname;
      requests.push({ path, method: request.method });
      if (request.method === 'POST') return Response.json({ id: 'already-created', createdAt: '2026-10-24T00:00:00Z', expiresAt: '2026-10-24T00:05:00Z' });
      return Response.json({ code: 'FSB::API_ERROR', message: secret }, { status: 429 });
    });
    const error = await openBinding(options(kind)).catch(error => error);
    expect(error).toBeInstanceOf(UnknownOutcomeError);
    expect(error).toMatchObject({ code: 'outcome_unknown', classification: 'api', statusCode: 429 });
    expect(requests.filter(request => request.method === 'POST')).toHaveLength(1);
    expect(requests.some(request => request.path.includes('/already-created/endpoints/'))).toBe(true);
    // SDK 1.1.1 attempts deletion internally, but its failure must not be called rollback.
    expect(requests.filter(request => request.method === 'DELETE')).toEqual([{ path: '/v1/sandboxes/already-created', method: 'DELETE' }]);
    assertSafe(error);
  });
});

// SDK 1.1.1 uses untyped Errors for these checks, so reject them before opening.
describe('local SDK option validation', () => {
  it.each([undefined, null, 0, 300])('preserves SDK-supported image TTL %s and absolute host paths', async timeoutSeconds => {
    const fake = fakeSandbox(); const sdk = fakeSdk(fake.sandbox);
    const valid = { ...options('create-image'), ...(timeoutSeconds === undefined ? {} : { timeoutSeconds }), volumes: [
      { name: 'linux', mountPath: '/linux', host: { path: '/data' } },
      { name: 'windows', mountPath: '/windows', host: { path: 'C:\\data' } },
      { name: 'pvc', mountPath: '/pvc', pvc: { claimName: 'data' } },
    ] } as OpenOptions;
    const binding = await createBindingOpener(sdk)(valid);
    expect(sdk.create).toHaveBeenCalledOnce();
    expect(sdk.create.mock.calls[0]?.[0].timeoutSeconds).toBe(timeoutSeconds);
    expect(sdk.create.mock.calls[0]?.[0].volumes).toEqual(valid.kind === 'create-image' ? valid.volumes : []);
    await binding.close();
  });

  it.each([
    { ...options('create-template'), templateId: '   ' },
    { ...options('create-image'), timeoutSeconds: Number.NaN },
    { ...options('create-image'), timeoutSeconds: Infinity },
    { ...options('create-image'), volumes: [{ name: secret, mountPath: '/data' }] },
    { ...options('create-image'), volumes: [{ name: secret, mountPath: '/data', host: { path: '/data' }, pvc: { claimName: 'data' } }] },
    { ...options('create-image'), volumes: [{ name: secret, mountPath: '/data', host: { path: secret } }] },
  ])('rejects invalid options without making an SDK or HTTP call', async invalid => {
    const sdk = fakeSdk(fakeSandbox().sandbox);
    const fetch = vi.fn<typeof globalThis.fetch>(); vi.stubGlobal('fetch', fetch);
    for (const open of [createBindingOpener(sdk), openBinding]) {
      const error = await open(invalid as OpenOptions).catch(error => error);
      expect(error).toMatchObject({ code: 'invalid_options' }); assertSafe(error);
    }
    expect(sdk.create).not.toHaveBeenCalled(); expect(sdk.createFromTemplate).not.toHaveBeenCalled(); expect(fetch).not.toHaveBeenCalled();
  });
});
