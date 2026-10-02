// Copyright 2026 The OpenSandbox Authors
// 
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// 
//     http://www.apache.org/licenses/LICENSE-2.0
// 
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import type {
  DiagnosticContentResponse,
  PaginatedSandboxes,
  K8sEventRow,
  K8sListResponse,
  K8sWorkloadRow,
  PlatformSummary,
  PoolListResponse,
  PoolItem,
  RuntimeStats,
  Sandbox,
  SessionUser,
  Snapshot,
  SnapshotListResponse,
} from './types';

export class ApiError extends Error {
  readonly code: string;
  readonly status: number;

  constructor(code: string, message: string, status: number) {
    super(message);
    this.code = code;
    this.status = status;
  }
}

let onUnauthorized: (() => void) | null = null;

export function setUnauthorizedHandler(handler: () => void) {
  onUnauthorized = handler;
}

async function parseBody(res: Response): Promise<unknown> {
  const text = await res.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return { message: text };
  }
}

function extractError(body: unknown, fallback: string): { code: string; message: string } {
  if (body && typeof body === 'object') {
    const record = body as Record<string, unknown>;
    const detail = record.detail;
    if (detail && typeof detail === 'object') {
      const d = detail as Record<string, unknown>;
      return {
        code: String(d.code ?? 'ERROR'),
        message: String(d.message ?? fallback),
      };
    }
    return {
      code: String(record.code ?? 'ERROR'),
      message: String(record.message ?? fallback),
    };
  }
  return { code: 'ERROR', message: fallback };
}

export async function apiRequest<T>(
  path: string,
  init: RequestInit = {},
): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }

  const res = await fetch(path, {
    ...init,
    credentials: 'include',
    headers,
  });

  if (res.status === 401) {
    onUnauthorized?.();
    throw new ApiError('UNAUTHORIZED', 'Session required', 401);
  }

  if (res.status === 204) {
    return undefined as T;
  }

  const body = await parseBody(res);
  if (!res.ok) {
    const err = extractError(body, res.statusText);
    throw new ApiError(err.code, err.message, res.status);
  }

  return body as T;
}

export function isNotFound(err: unknown): boolean {
  return err instanceof ApiError && err.status === 404;
}

export const authApi = {
  me: () => apiRequest<SessionUser>('/api/auth/me'),
  loginTenant: (apiKey: string) =>
    apiRequest<SessionUser>('/api/auth/tenant', {
      method: 'POST',
      body: JSON.stringify({ apiKey }),
    }),
  loginAdmin: (adminToken: string) =>
    apiRequest<SessionUser>('/api/auth/admin', {
      method: 'POST',
      body: JSON.stringify({ adminToken }),
    }),
  logout: () => apiRequest<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
};

export const sandboxApi = {
  list: (query: Record<string, string | number | undefined>) => {
    const params = new URLSearchParams();
    Object.entries(query).forEach(([k, v]) => {
      if (v !== undefined && v !== '') params.set(k, String(v));
    });
    const qs = params.toString();
    return apiRequest<PaginatedSandboxes>(`/api/sandboxes${qs ? `?${qs}` : ''}`);
  },
  get: (id: string) => apiRequest<Sandbox>(`/api/sandboxes/${encodeURIComponent(id)}`),
  create: (body: Record<string, unknown>) =>
    apiRequest<Sandbox>('/api/sandboxes', { method: 'POST', body: JSON.stringify(body) }),
  remove: (id: string) =>
    apiRequest<void>(`/api/sandboxes/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  renewExpiration: (id: string, expiresAt: string) =>
    apiRequest<{ expiresAt: string }>(
      `/api/sandboxes/${encodeURIComponent(id)}/renew-expiration`,
      { method: 'POST', body: JSON.stringify({ expiresAt }) },
    ),
  pause: (id: string) =>
    apiRequest<unknown>(`/api/sandboxes/${encodeURIComponent(id)}/pause`, { method: 'POST' }),
  resume: (id: string) =>
    apiRequest<unknown>(`/api/sandboxes/${encodeURIComponent(id)}/resume`, { method: 'POST' }),
  endpoint: (id: string, port: number) =>
    apiRequest<Record<string, unknown>>(
      `/api/sandboxes/${encodeURIComponent(id)}/endpoints/${port}`,
    ),
  diagnosticLogs: (id: string, scope: string) =>
    apiRequest<DiagnosticContentResponse>(
      `/api/sandboxes/${encodeURIComponent(id)}/diagnostics/logs?scope=${encodeURIComponent(scope)}`,
    ),
  diagnosticEvents: (id: string, scope: string) =>
    apiRequest<DiagnosticContentResponse>(
      `/api/sandboxes/${encodeURIComponent(id)}/diagnostics/events?scope=${encodeURIComponent(scope)}`,
    ),
  archiveLogs: (id: string, maxBytes?: number) => {
    const params = new URLSearchParams();
    if (maxBytes) params.set('maxBytes', String(maxBytes));
    const qs = params.toString();
    return apiRequest<DiagnosticContentResponse>(
      `/api/sandboxes/${encodeURIComponent(id)}/logs/archive${qs ? `?${qs}` : ''}`,
    );
  },
  createSnapshot: (id: string, body?: { name?: string }) =>
    apiRequest<Snapshot>(`/api/sandboxes/${encodeURIComponent(id)}/snapshots`, {
      method: 'POST',
      body: JSON.stringify(body ?? {}),
    }),
};

export const adminApi = {
  diagnosticLogs: (sandboxId: string, tenant: string, scope: string) => {
    const params = new URLSearchParams({ tenant, scope });
    return apiRequest<DiagnosticContentResponse>(
      `/api/admin/sandboxes/${encodeURIComponent(sandboxId)}/diagnostics/logs?${params}`,
    );
  },
  diagnosticEvents: (sandboxId: string, tenant: string, scope: string) => {
    const params = new URLSearchParams({ tenant, scope });
    return apiRequest<DiagnosticContentResponse>(
      `/api/admin/sandboxes/${encodeURIComponent(sandboxId)}/diagnostics/events?${params}`,
    );
  },
  archiveLogs: (sandboxId: string, tenant: string, maxBytes?: number) => {
    const params = new URLSearchParams({ tenant });
    if (maxBytes) params.set('maxBytes', String(maxBytes));
    return apiRequest<DiagnosticContentResponse>(
      `/api/admin/sandboxes/${encodeURIComponent(sandboxId)}/logs/archive?${params}`,
    );
  },
  listTenants: () =>
    apiRequest<{ items: { name: string; namespace?: string }[] }>('/api/admin/tenants'),
  sandboxes: (query: Record<string, string | number | undefined>) => {
    const params = new URLSearchParams();
    Object.entries(query).forEach(([k, v]) => {
      if (v !== undefined && v !== '') params.set(k, String(v));
    });
    const qs = params.toString();
    return apiRequest<PaginatedSandboxes>(`/api/admin/sandboxes${qs ? `?${qs}` : ''}`);
  },
  runtimeStats: () => apiRequest<RuntimeStats>('/api/admin/stats/runtime'),
  platformSummary: () => apiRequest<PlatformSummary>('/api/admin/platform/summary'),
  getSandbox: (id: string, tenant: string) =>
    apiRequest<Sandbox>(
      `/api/admin/sandboxes/${encodeURIComponent(id)}?tenant=${encodeURIComponent(tenant)}`,
    ),
  removeSandbox: (id: string, tenant: string) =>
    apiRequest<void>(
      `/api/admin/sandboxes/${encodeURIComponent(id)}?tenant=${encodeURIComponent(tenant)}`,
      { method: 'DELETE' },
    ),
  renewSandbox: (id: string, tenant: string, expiresAt: string) =>
    apiRequest<{ expiresAt: string }>(
      `/api/admin/sandboxes/${encodeURIComponent(id)}/renew-expiration?tenant=${encodeURIComponent(tenant)}`,
      { method: 'POST', body: JSON.stringify({ expiresAt }) },
    ),
  pauseSandbox: (id: string, tenant: string) =>
    apiRequest<unknown>(
      `/api/admin/sandboxes/${encodeURIComponent(id)}/pause?tenant=${encodeURIComponent(tenant)}`,
      { method: 'POST' },
    ),
  resumeSandbox: (id: string, tenant: string) =>
    apiRequest<unknown>(
      `/api/admin/sandboxes/${encodeURIComponent(id)}/resume?tenant=${encodeURIComponent(tenant)}`,
      { method: 'POST' },
    ),
  sandboxEndpoint: (id: string, tenant: string, port: number) =>
    apiRequest<Record<string, unknown>>(
      `/api/admin/sandboxes/${encodeURIComponent(id)}/endpoints/${port}?tenant=${encodeURIComponent(tenant)}`,
    ),
  createSnapshot: (sandboxId: string, tenant: string, body?: { name?: string }) => {
    const params = new URLSearchParams({ tenant });
    return apiRequest<Snapshot>(
      `/api/admin/sandboxes/${encodeURIComponent(sandboxId)}/snapshots?${params}`,
      { method: 'POST', body: JSON.stringify(body ?? {}) },
    );
  },
  listSnapshots: (tenant: string) =>
    apiRequest<SnapshotListResponse>(
      `/api/admin/snapshots?tenant=${encodeURIComponent(tenant)}`,
    ),
  deleteSnapshot: (snapshotId: string, tenant: string) =>
    apiRequest<void>(
      `/api/admin/snapshots/${encodeURIComponent(snapshotId)}?tenant=${encodeURIComponent(tenant)}`,
      { method: 'DELETE' },
    ),
  k8sWorkloads: (query: Record<string, string | undefined>) => {
    const params = new URLSearchParams();
    Object.entries(query).forEach(([k, v]) => {
      if (v) params.set(k, v);
    });
    const qs = params.toString();
    return apiRequest<K8sListResponse<K8sWorkloadRow>>(
      `/api/admin/k8s/workloads${qs ? `?${qs}` : ''}`,
    );
  },
  k8sEvents: (query: Record<string, string | undefined>) => {
    const params = new URLSearchParams();
    Object.entries(query).forEach(([k, v]) => {
      if (v) params.set(k, v);
    });
    const qs = params.toString();
    return apiRequest<K8sListResponse<K8sEventRow>>(
      `/api/admin/k8s/events${qs ? `?${qs}` : ''}`,
    );
  },
};

export const snapshotApi = {
  list: (query?: Record<string, string>) => {
    const params = new URLSearchParams(query);
    const qs = params.toString();
    return apiRequest<SnapshotListResponse>(`/api/snapshots${qs ? `?${qs}` : ''}`);
  },
  remove: (id: string) =>
    apiRequest<void>(`/api/snapshots/${encodeURIComponent(id)}`, { method: 'DELETE' }),
};

export const poolApi = {
  list: () => apiRequest<PoolListResponse>('/api/pools'),
  get: (name: string) => apiRequest<PoolItem>(`/api/pools/${encodeURIComponent(name)}`),
  create: (body: Record<string, unknown>) =>
    apiRequest<PoolItem>('/api/pools', { method: 'POST', body: JSON.stringify(body) }),
  update: (name: string, body: Record<string, unknown>) =>
    apiRequest<PoolItem>(`/api/pools/${encodeURIComponent(name)}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  remove: (name: string) =>
    apiRequest<void>(`/api/pools/${encodeURIComponent(name)}`, { method: 'DELETE' }),
};

export const platformApi = {
  version: () => apiRequest<Record<string, unknown>>('/api/version'),
  bffHealth: () => apiRequest<{ status: string }>('/health'),
};

export const historyApi = {
  getSandbox: (sandboxId: string, tenant?: string) => {
    const qs = tenant ? `?tenant=${encodeURIComponent(tenant)}` : '';
    return apiRequest<import('./types').SandboxHistoryItem>(
      `/api/history/sandboxes/${encodeURIComponent(sandboxId)}${qs}`,
    );
  },
  listSandboxes: (query?: Record<string, string | number | boolean | undefined>) => {
    const params = new URLSearchParams();
    Object.entries(query ?? {}).forEach(([k, v]) => {
      if (v !== undefined && v !== '') params.set(k, String(v));
    });
    const qs = params.toString();
    return apiRequest<import('./types').SandboxHistoryListResponse>(
      `/api/history/sandboxes${qs ? `?${qs}` : ''}`,
    );
  },
  stats: (tenant?: string) => {
    const qs = tenant ? `?tenant=${encodeURIComponent(tenant)}` : '';
    return apiRequest<import('./types').SandboxHistoryStats>(`/api/history/stats${qs}`);
  },
  usage: (query: { from: string; to: string; tenant?: string }) => {
    const params = new URLSearchParams({ from: query.from, to: query.to });
    if (query.tenant) params.set('tenant', query.tenant);
    return apiRequest<import('./types').TenantUsageResponse>(`/api/history/usage?${params}`);
  },
  imageStats: (query?: { from?: string; to?: string; tenant?: string }) => {
    const params = new URLSearchParams();
    if (query?.from) params.set('from', query.from);
    if (query?.to) params.set('to', query.to);
    if (query?.tenant) params.set('tenant', query.tenant);
    const qs = params.toString();
    return apiRequest<import('./types').ImageStatsResponse>(
      `/api/history/images/stats${qs ? `?${qs}` : ''}`,
    );
  },
};
