// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest';
import { openBinding } from '../src/binding.js';
import type { OpenOptions } from '../src/types.js';

/** Published SDK/default adapters over in-memory HTTP; no server or sandbox. */
function coldStartHttp(unavailablePings: number) {
  const requests: { method: string; path: string }[] = [];
  let pings = 0;
  let prematureCommands = 0;
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const request = new Request(input, init); const path = new URL(request.url).pathname;
    requests.push({ method: request.method, path });
    if (path.includes('/endpoints/')) {
      return Response.json({ endpoint: 'offline.invalid/restored' }, { headers: { 'OPEN-SANDBOX-ORIGIN': 'template' } });
    }
    if (request.method === 'DELETE') return new Response(null, { status: 204 });
    if (path.endsWith('/ping')) {
      pings++;
      // Ingress can return an empty 503 while the restored execd port is unavailable.
      return pings <= unavailablePings ? new Response(null, { status: 503 }) : new Response('pong');
    }
    if (path.endsWith('/command')) {
      if (pings <= unavailablePings) {
        prematureCommands++;
        return new Response(null, { status: 503 });
      }
      return new Response('data: {"type":"init","text":"preflight"}\n\ndata: {"type":"execution_complete","execution_time":1}\n\n',
        { headers: { 'content-type': 'text/event-stream' } });
    }
    if (request.method === 'POST' && (path === '/v1/sandboxes' || path === '/v1/sandboxes/from-template')) {
      return Response.json({ id: 'restored', createdAt: '2026-10-24T00:00:00Z', expiresAt: '2026-10-24T00:05:00Z' });
    }
    throw new Error('Unexpected offline readiness request.');
  });
  return { requests, get pings() { return pings; }, get prematureCommands() { return prematureCommands; } };
}
function options(kind: 'create-image' | 'create-template' | 'connect'): OpenOptions {
  return {
    ...(kind === 'create-image' ? { kind, image: 'python:3.11' } : kind === 'create-template'
      ? { kind, templateId: 'snapshot-template', timeoutSeconds: 300 } : { kind, sandboxId: 'restored' }),
    sessionId: 'cold-start', remoteCwd: '/workspace', readyTimeoutSeconds: 1, healthCheckPollingInterval: 1,
    connectionConfig: { domain: 'offline.invalid', disableMetrics: true },
  };
}
afterEach(() => vi.unstubAllGlobals());

describe('empty-response cold-start readiness', () => {
  it.each(['create-image', 'create-template', 'connect'] as const)('waits through empty-body HTTP 503 before preflight for %s', async kind => {
    const http = coldStartHttp(2);
    const binding = await openBinding(options(kind));
    try {
      expect(binding.state).toBe('open');
      expect(http.pings).toBe(3); expect(http.prematureCommands).toBe(0);
      expect(http.requests.filter(request => request.path.endsWith('/command'))).toHaveLength(1);
      expect(http.requests.filter(request => request.method === 'DELETE')).toHaveLength(0);
      expect(http.requests.filter(request => request.method === 'POST' && !request.path.endsWith('/command'))).toHaveLength(kind === 'connect' ? 0 : 1);
    } finally { await binding.close(); }
  });

  it.each(['create-image', 'create-template', 'connect'] as const)('keeps persistent empty-body HTTP 503 in the readiness stage for %s', async kind => {
    const http = coldStartHttp(Infinity);
    const error = await openBinding({ ...options(kind), readyTimeoutSeconds: 0.2 }).catch(error => error);
    expect(error).toMatchObject({ code: 'binding_open_failed', stage: 'readiness', sandboxId: 'restored',
      cleanupState: kind === 'connect' ? 'not-owned' : 'killed' });
    expect(http.pings).toBeGreaterThan(0); expect(http.prematureCommands).toBe(0);
    expect(http.requests.filter(request => request.path.endsWith('/command'))).toHaveLength(0);
    expect(http.requests.filter(request => request.method === 'DELETE')).toHaveLength(kind === 'connect' ? 0 : 1);
    expect(http.requests.filter(request => request.method === 'POST')).toHaveLength(kind === 'connect' ? 0 : 1);
  });
});
