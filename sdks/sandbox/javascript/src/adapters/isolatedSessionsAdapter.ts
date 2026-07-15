// Copyright 2026 Alibaba Group Holding Ltd.
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

import { createExecdClient } from "../openapi/execdClient.js";
import { IsolatedFilesystemAdapter } from "./isolatedFilesystemAdapter.js";
import { parseJsonEventStream } from "./sse.js";
import type { CommandExecution, ServerStreamEvent } from "../models/execd.js";
import type { ExecutionHandlers } from "../models/execution.js";
import { ExecutionEventDispatcher } from "../models/executionEventDispatcher.js";
import type { SandboxFiles } from "../services/filesystem.js";
import type {
  IsolationService,
  IsolationSession,
  RunOnceOpts,
} from "../services/isolatedSessions.js";
import type {
  CreateIsolatedSessionRequest,
  IsolatedCapabilities,
  IsolatedRunOpts,
  IsolatedSessionInfo,
  IsolatedSessionState,
  IsolatedSessionSummary,
  ListIsolatedSessionsResponse,
} from "../models/isolated.js";

function joinUrl(baseUrl: string, pathname: string): string {
  const base = baseUrl.endsWith("/") ? baseUrl.slice(0, -1) : baseUrl;
  const path = pathname.startsWith("/") ? pathname : `/${pathname}`;
  return `${base}${path}`;
}

function assertNonBlank(value: string, field: string): void {
  if (!value.trim()) {
    throw new Error(`${field} cannot be empty`);
  }
}

function inferExitCode(execution: CommandExecution): number | null {
  const errorValue = execution.error?.value?.trim();
  const parsedExitCode =
    errorValue && /^-?\d+$/.test(errorValue) ? Number(errorValue) : Number.NaN;
  return execution.error != null
    ? (Number.isFinite(parsedExitCode) ? parsedExitCode : null)
    : execution.complete
      ? 0
      : null;
}

export interface IsolatedSessionsAdapterOptions {
  baseUrl: string;
  fetch?: typeof fetch;
  /** Unbounded-timeout fetch for SSE streaming (run endpoint). Falls back to `fetch`. */
  sseFetch?: typeof fetch;
  headers?: Record<string, string>;
}

class IsolationSessionHandle implements IsolationSession {
  private _files: SandboxFiles | undefined;

  constructor(
    private readonly _info: IsolatedSessionInfo,
    private readonly adapter: IsolatedSessionsAdapter,
  ) {}

  get sessionId(): string { return this._info.session_id; }
  get info(): IsolatedSessionInfo { return this._info; }
  get files(): SandboxFiles {
    if (!this._files) {
      const client = createExecdClient({
        baseUrl: this.adapter.opts.baseUrl,
        headers: this.adapter.opts.headers,
        fetch: this.adapter.opts.fetch,
      });
      this._files = new IsolatedFilesystemAdapter(client, {
        baseUrl: this.adapter.opts.baseUrl,
        sessionId: this._info.session_id,
        fetch: this.adapter.opts.fetch,
        headers: this.adapter.opts.headers,
      });
    }
    return this._files;
  }

  run(code: string, opts?: IsolatedRunOpts, handlers?: ExecutionHandlers, signal?: AbortSignal): Promise<CommandExecution> {
    return this.adapter._run(this._info.session_id, code, opts, handlers, signal);
  }
  get(): Promise<IsolatedSessionState> {
    return this.adapter._get(this._info.session_id);
  }
  delete(): Promise<void> {
    return this.adapter._delete(this._info.session_id);
  }
}

export class IsolatedSessionsAdapter implements IsolationService {
  private readonly fetch: typeof fetch;
  private readonly sseFetch: typeof fetch;

  constructor(readonly opts: IsolatedSessionsAdapterOptions) {
    this.fetch = opts.fetch ?? fetch;
    this.sseFetch = opts.sseFetch ?? this.fetch;
  }

  private async jsonRequest<T>(
    method: string,
    pathname: string,
    body?: unknown,
  ): Promise<T> {
    const url = joinUrl(this.opts.baseUrl, pathname);
    const headers: Record<string, string> = {
      "content-type": "application/json",
      accept: "application/json",
      ...(this.opts.headers ?? {}),
    };
    const res = await this.fetch(url, {
      method,
      headers,
      body: body != null ? JSON.stringify(body) : undefined,
    });
    if (!res.ok) {
      const text = await res.text().catch(() => "");
      throw new Error(`${method} ${pathname} failed: ${res.status} ${text}`);
    }
    if (res.status === 204) return undefined as T;
    const text = await res.text();
    if (!text) return undefined as T;
    return JSON.parse(text) as T;
  }

  async create(request: CreateIsolatedSessionRequest): Promise<IsolationSessionHandle> {
    const info = await this.jsonRequest<IsolatedSessionInfo>(
      "POST",
      "/v1/isolated/session",
      request,
    );
    return new IsolationSessionHandle(info, this);
  }

  async _get(sessionId: string): Promise<IsolatedSessionState> {
    assertNonBlank(sessionId, "sessionId");
    return this.jsonRequest<IsolatedSessionState>(
      "GET",
      `/v1/isolated/session/${encodeURIComponent(sessionId)}`,
    );
  }

  async _run(
    sessionId: string,
    code: string,
    opts?: IsolatedRunOpts,
    handlers?: ExecutionHandlers,
    signal?: AbortSignal,
  ): Promise<CommandExecution> {
    assertNonBlank(sessionId, "sessionId");
    assertNonBlank(code, "code");

    const body: Record<string, unknown> = { code };
    if (opts?.envs) body.envs = opts.envs;
    if (opts?.timeout_seconds != null) body.timeout_seconds = opts.timeout_seconds;

    const url = joinUrl(
      this.opts.baseUrl,
      `/v1/isolated/session/${encodeURIComponent(sessionId)}/run`,
    );
    const res = await this.sseFetch(url, {
      method: "POST",
      headers: {
        accept: "text/event-stream",
        "content-type": "application/json",
        ...(this.opts.headers ?? {}),
      },
      body: JSON.stringify(body),
      signal,
    });

    const execution: CommandExecution = {
      logs: { stdout: [], stderr: [] },
      result: [],
    };
    const dispatcher = new ExecutionEventDispatcher(execution, handlers);

    for await (const ev of parseJsonEventStream<ServerStreamEvent>(res, {
      fallbackErrorMessage: "Run in isolated session failed",
    })) {
      await dispatcher.dispatch(ev as any);
    }

    execution.exitCode = inferExitCode(execution);
    return execution;
  }

  async _delete(sessionId: string): Promise<void> {
    assertNonBlank(sessionId, "sessionId");
    await this.jsonRequest<void>(
      "DELETE",
      `/v1/isolated/session/${encodeURIComponent(sessionId)}`,
    );
  }

  async capabilities(): Promise<IsolatedCapabilities> {
    return this.jsonRequest<IsolatedCapabilities>(
      "GET",
      "/v1/isolated/capabilities",
    );
  }

  async list(): Promise<IsolatedSessionSummary[]> {
    const resp = await this.jsonRequest<ListIsolatedSessionsResponse>(
      "GET",
      "/v1/isolated/sessions",
    );
    return resp.sessions ?? [];
  }

  async runOnce(
    code: string,
    workspace: string,
    opts?: RunOnceOpts,
  ): Promise<CommandExecution> {
    const session = await this.create({
      workspace: { path: workspace, mode: opts?.workspaceMode },
      profile: opts?.profile,
      share_net: opts?.shareNet,
      binds: opts?.binds,
    });
    try {
      return await session.run(code, opts?.runOpts, opts?.handlers, opts?.signal);
    } finally {
      try { await session.delete(); } catch { /* best-effort cleanup */ }
    }
  }

  async withSession<T>(
    request: CreateIsolatedSessionRequest,
    fn: (session: IsolationSession) => Promise<T>,
  ): Promise<T> {
    const session = await this.create(request);
    try {
      return await fn(session);
    } finally {
      try { await session.delete(); } catch { /* best-effort cleanup */ }
    }
  }
}
