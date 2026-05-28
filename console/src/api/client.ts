/**
 * Lifecycle API client (user-auth path: no API key in the browser; proxy injects headers).
 */

const API_PREFIX = (import.meta.env.VITE_API_PREFIX as string | undefined) ?? "/v1";

export type ApiErrorBody = { code: string; message: string };

export class ApiError extends Error {
  constructor(
    public status: number,
    public body: ApiErrorBody,
  ) {
    super(body.message);
    this.name = "ApiError";
  }
}

export interface SandboxListItem {
  id: string;
  status: { state: string; reason?: string; message?: string };
  metadata?: Record<string, string>;
  image: { uri: string };
  expiresAt: string;
  createdAt: string;
  entrypoint: string[];
}

export interface ListResponse {
  items: SandboxListItem[];
  pagination?: {
    page: number;
    pageSize: number;
    totalItems: number;
    totalPages: number;
    hasNextPage: boolean;
  };
}

async function parseJson(res: Response): Promise<{ data: unknown; text: string }> {
  const text = await res.text();
  if (!text) {
    return { data: null, text: "" };
  }
  try {
    return { data: JSON.parse(text) as unknown, text };
  } catch {
    return { data: null, text };
  }
}

export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_PREFIX}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...(init?.headers ?? {}),
    },
  });
  if (res.status === 204 || res.status === 205) {
    return null as T;
  }
  const parsed = await parseJson(res);
  const data = parsed.data as Record<string, unknown> | null;
  if (!res.ok) {
    const code = typeof data?.code === "string" ? data.code : "HTTP_ERROR";
    let message = typeof data?.message === "string" ? data.message : "";
    if (!message && parsed.text && parsed.text.length < 240) {
      message = parsed.text;
    }
    if (!message && res.status === 500) {
      message = "The API returned an internal server error (500). Check the server and proxy logs for details.";
    }
    if (!message) {
      message = res.statusText || `HTTP ${res.status}`;
    }
    throw new ApiError(res.status, { code, message });
  }
  return data as T;
}

export function listSandboxes(params: { state?: string; metadata?: string; page?: number; pageSize?: number }) {
  const q = new URLSearchParams();
  if (params.state) {
    q.set("state", params.state);
  }
  if (params.metadata) {
    q.set("metadata", params.metadata);
  }
  if (params.page != null) {
    q.set("page", String(params.page));
  }
  if (params.pageSize != null) {
    q.set("pageSize", String(params.pageSize));
  }
  const qs = q.toString();
  return apiFetch<ListResponse>(`/sandboxes${qs ? `?${qs}` : ""}`);
}

export function getSandbox(id: string) {
  return apiFetch<SandboxListItem>(`/sandboxes/${encodeURIComponent(id)}`);
}

export interface CreatePayload {
  image: { uri: string };
  timeout: number;
  resourceLimits: Record<string, string>;
  entrypoint: string[];
  env?: Record<string, string | null | undefined> | null;
  metadata?: Record<string, string>;
}

export function createSandbox(body: CreatePayload) {
  return apiFetch<{
    id: string;
    status: { state: string };
    metadata?: Record<string, string>;
    expiresAt: string;
  }>("/sandboxes", { method: "POST", body: JSON.stringify(body) });
}

export function deleteSandbox(id: string) {
  return apiFetch<null>(`/sandboxes/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export function renewExpiration(id: string, expiresAt: string) {
  return apiFetch<{ expiresAt: string }>(`/sandboxes/${encodeURIComponent(id)}/renew-expiration`, {
    method: "POST",
    body: JSON.stringify({ expiresAt }),
  });
}

export function getEndpoint(id: string, port: number, useServerProxy = false) {
  return apiFetch<{ endpoint: string }>(
    `/sandboxes/${encodeURIComponent(id)}/endpoints/${port}?use_server_proxy=${useServerProxy ? "true" : "false"}`,
  );
}

export function pauseSandbox(id: string) {
  return apiFetch<null>(`/sandboxes/${encodeURIComponent(id)}/pause`, { method: "POST" });
}

export function resumeSandbox(id: string) {
  return apiFetch<null>(`/sandboxes/${encodeURIComponent(id)}/resume`, { method: "POST" });
}
