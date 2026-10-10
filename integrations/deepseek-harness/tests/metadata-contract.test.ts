// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

// OFFLINE source/wire contract: actual published SDK, extracted server validator,
// in-memory HTTP only. This does not establish deployed FastPath acceptance.
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { SandboxManager } from '@alibaba-group/opensandbox';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { openBinding } from '../src/binding.js';
import { UnknownOutcomeError } from '../src/errors.js';
import type { OpenOptions } from '../src/types.js';

const key = 'dsh-binding-request';
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const serverSource = new URL('../../../server/opensandbox_server/services/fast_sandbox/create_mapping.py', import.meta.url);
type Contract = { accepted: boolean; field?: string; reason?: string; sourceSha256: string; mappings: string[] };
function validateMetadata(metadata: Record<string, string>): Contract {
  const result = spawnSync('python3', [fileURLToPath(new URL('./fixtures/server-metadata-contract.py', import.meta.url)), fileURLToPath(serverSource)], {
    input: JSON.stringify(metadata), encoding: 'utf8', timeout: 10_000,
  });
  expect(result.error).toBeUndefined();
  expect(result.status, result.stderr).toBe(0);
  const contract = JSON.parse(result.stdout) as Contract;
  expect(contract.sourceSha256).toBe(createHash('sha256').update(readFileSync(serverSource)).digest('hex'));
  expect(contract.mappings).toEqual(['map_create_request', 'map_template_create_request']);
  return contract;
}

function wireFixture(lostPost = false) {
  const creates: { body: Record<string, unknown> & { metadata: Record<string, string> }; contract: Contract }[] = [];
  const requests: { method: string; path: string }[] = [];
  const filters: Record<string, string>[] = [];
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init), url = new URL(request.url);
    requests.push({ method: request.method, path: url.pathname });
    if (url.pathname === '/v1/sandboxes' && request.method === 'POST') {
      const body = await request.json() as typeof creates[number]['body'];
      const contract = validateMetadata(body.metadata); creates.push({ body, contract });
      if (lostPost) throw new Error('lost POST response with private-key-do-not-publish');
      if (!contract.accepted) return Response.json({ code: 'BAD_REQUEST', message: contract.reason }, { status: 400 });
      return Response.json({ id: 'metadata-contract', createdAt: '2026-10-09T00:00:00Z', expiresAt: '2026-10-09T00:10:00Z' });
    }
    if (url.pathname === '/v1/sandboxes' && request.method === 'GET') {
      const metadata = Object.fromEntries(new URLSearchParams(url.searchParams.get('metadata') ?? ''));
      filters.push(metadata);
      // Current OpenSandbox lists/filter Sandbox CRs; this checks SDK query
      // serialization, without imposing the raw FastPath List validator here.
      return Response.json({ items: [], pagination: { page: 1, pageSize: 20, totalItems: 0, totalPages: 0 } });
    }
    if (url.pathname.includes('/endpoints/')) return Response.json({ endpoint: 'offline.invalid/metadata-contract' }, { headers: { 'OPEN-SANDBOX-ORIGIN': 'template' } });
    if (url.pathname.endsWith('/ping')) return new Response('pong');
    if (url.pathname.endsWith('/command') && request.method === 'POST') return new Response([
      'data: {"type":"init","text":"metadata-preflight"}\n\n',
      'data: {"type":"execution_complete","execution_time":1}\n\n',
    ].join(''), { headers: { 'content-type': 'text/event-stream' } });
    throw new Error(`Unexpected offline metadata endpoint: ${request.method} ${url.pathname}`);
  });
  return { creates, requests, filters };
}

function options(kind: 'create-image' | 'create-template', metadata: Record<string, string>): OpenOptions {
  const common = {
    sessionId: 'metadata-session', remoteCwd: '/workspace', timeoutSeconds: 600, metadata,
    connectionConfig: { domain: 'offline.invalid', apiKey: 'private-key-do-not-publish', disableMetrics: true } };
  return kind === 'create-image' ? { ...common, kind, image: 'python:3.11' } : { ...common, kind, templateId: 'metadata-template' };
}
afterEach(() => vi.unstubAllGlobals());

describe('FastPath correlation metadata source and published SDK wire contract', () => {
  it('rejects the historical dotted key using the actual server validator', () => {
    expect(validateMetadata({ 'dsh.binding.request': '9e8aef4c-1ed7-4b85-a824-ac56c0546daa' }))
      .toMatchObject({ accepted: false, field: 'metadata', reason: expect.stringContaining('must be a DNS label') });
  });
  it('accepts the reserved hyphenated key and UUID value using the actual server validator', () => {
    expect(validateMetadata({ [key]: '9e8aef4c-1ed7-4b85-a824-ac56c0546daa' })).toMatchObject({ accepted: true });
  });
  it.each(['create-image', 'create-template'] as const)('forwards a valid private correlation tag in %s POST and manager lookup', async kind => {
    const wire = wireFixture(), metadata = Object.freeze({ project: 'wire-contract', [key]: 'caller-reserved-value' });
    const result = await openBinding(options(kind, metadata)).catch(error => error);
    expect(wire.creates).toHaveLength(1);
    expect(wire.creates[0]!.contract.accepted, wire.creates[0]!.contract.reason).toBe(true);
    const body = wire.creates[0]!.body, correlationId = body.metadata[key]!;
    expect(correlationId).toMatch(uuid);
    expect(body.metadata).toEqual({ project: 'wire-contract', [key]: correlationId });
    expect(metadata).toEqual({ project: 'wire-contract', [key]: 'caller-reserved-value' });
    if (kind === 'create-image') expect(body.image).toEqual({ uri: 'python:3.11' });
    else expect(body.templateId).toBe('metadata-template');
    expect(result).toMatchObject({ descriptor: { sandboxId: 'metadata-contract' } });
    const manager = SandboxManager.create({ connectionConfig: { domain: 'offline.invalid', disableMetrics: true } });
    try { await manager.listSandboxInfos({ metadata: { [key]: correlationId } }); }
    finally { await manager.close(); await result.close(); }
    expect(wire.filters).toEqual([{ [key]: correlationId }]);
    expect(JSON.stringify(result)).not.toContain('private-key-do-not-publish');
  });
  it.each(['create-image', 'create-template'] as const)('retains the valid %s POST UUID on a lost response and never retries', async kind => {
    const wire = wireFixture(true), metadata = Object.freeze({ project: 'wire-contract' });
    const failure = await openBinding(options(kind, metadata)).catch(error => error);
    expect(wire.creates).toHaveLength(1);
    expect(wire.creates[0]!.contract.accepted, wire.creates[0]!.contract.reason).toBe(true);
    const correlationId = wire.creates[0]!.body.metadata[key];
    expect(correlationId).toMatch(uuid);
    expect(failure).toBeInstanceOf(UnknownOutcomeError);
    expect(failure).toMatchObject({ operation: 'create', code: 'outcome_unknown', correlationId });
    expect(wire.requests).toEqual([{ method: 'POST', path: '/v1/sandboxes' }]);
    expect(metadata).toEqual({ project: 'wire-contract' });
    expect(String(failure)).not.toContain('private-key-do-not-publish');
  });
  it.each(['create-image', 'create-template'] as const)('leaves caller metadata unchanged for %s and lets the server reject it', async kind => {
    const wire = wireFixture(), metadata = Object.freeze({ 'caller.namespace': 'retained-raw' });
    const failure = await openBinding(options(kind, metadata)).catch(error => error);
    expect(wire.creates).toHaveLength(1);
    expect(wire.creates[0]!.body.metadata).toMatchObject(metadata);
    expect(metadata).toEqual({ 'caller.namespace': 'retained-raw' });
    expect(wire.creates[0]!.contract).toMatchObject({ accepted: false, field: 'metadata' });
    expect(failure).toBeInstanceOf(UnknownOutcomeError);
    expect(wire.requests).toEqual([{ method: 'POST', path: '/v1/sandboxes' }]);
  });
});
