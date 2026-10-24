// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

import { Sandbox } from '@alibaba-group/opensandbox';
import { vi } from 'vitest';

/** Real published SDK with an in-memory fetch boundary: no DNS, sockets or sandbox. */
export function offlineSdkHttp() {
  const requests: { url: string; method: string }[] = [];
  let closedAttempts = 0;
  let failure: { status: number; body: string } | undefined;
  const fetchBoundary: typeof fetch = async (input, init) => {
    const dispatcher = (init as RequestInit & { dispatcher?: { closed?: boolean } })?.dispatcher;
    if (dispatcher?.closed) {
      closedAttempts++;
      throw new Error('offline fixture: closed dispatcher');
    }
    const url = input instanceof Request ? input.url : String(input);
    const method = init?.method ?? (input instanceof Request ? input.method : 'GET');
    requests.push({ url, method });
    if (url.includes('/endpoints/')) {
      const id = /\/sandboxes\/([^/]+)\//.exec(url)?.[1] ?? 'unknown';
      return Response.json({ endpoint: `offline.invalid/${id}` }, { headers: { 'OPEN-SANDBOX-ORIGIN': 'template' } });
    }
    if (url.endsWith('/ping')) return new Response('pong');
    if (failure) return new Response(failure.body, { status: failure.status });
    if (url.endsWith('/command') && method === 'POST') {
      return new Response([
        'data: {"type":"init","text":"offline-command"}\n\n',
        'data: {"type":"stdout","text":"offline-output"}\n\n',
        'data: {"type":"execution_complete","execution_time":1}\n\n',
      ].join(''), { headers: { 'content-type': 'text/event-stream' } });
    }
    if (method === 'DELETE') return new Response(null, { status: 204 });
    return Response.json({});
  };
  vi.stubGlobal('fetch', fetchBoundary);
  return {
    requests,
    get closedAttempts() { return closedAttempts; },
    fail(status: number, body: string) { failure = { status, body }; },
    async connect(id: string) {
      return Sandbox.connect({ sandboxId: id, skipHealthCheck: true,
        connectionConfig: { domain: 'offline.invalid', disableMetrics: true } });
    },
  };
}
