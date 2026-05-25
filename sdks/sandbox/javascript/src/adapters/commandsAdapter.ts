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

import type { ExecdClient } from "../openapi/execdClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import { parseJsonEventStream } from "./sse.js";
import type { paths as ExecdPaths } from "../api/execd.js";
import type {
  CommandExecution,
  CommandLogs,
  CommandStatus,
  RunCommandOpts,
  ServerStreamEvent,
} from "../models/execd.js";
import type { ExecdCommands } from "../services/execdCommands.js";
import type { ExecutionHandlers } from "../models/execution.js";
import { ExecutionEventDispatcher } from "../models/executionEventDispatcher.js";

function joinUrl(baseUrl: string, pathname: string): string {
  const base = baseUrl.endsWith("/") ? baseUrl.slice(0, -1) : baseUrl;
  const path = pathname.startsWith("/") ? pathname : `/${pathname}`;
  return `${base}${path}`;
}

/** Request body for POST /command (from generated spec; includes uid, gid, envs). */
type ApiRunCommandRequest =
  ExecdPaths["/command"]["post"]["requestBody"]["content"]["application/json"];
type ApiCommandStatusOk =
  ExecdPaths["/command/status/{id}"]["get"]["responses"][200]["content"]["application/json"];
type ApiCommandLogsOk =
  ExecdPaths["/command/{id}/logs"]["get"]["responses"][200]["content"]["text/plain"];
type ApiCreateSessionRequest =
  NonNullable<ExecdPaths["/session"]["post"]["requestBody"]>["content"]["application/json"];
type ApiCreateSessionOk =
  ExecdPaths["/session"]["post"]["responses"][200]["content"]["application/json"];
type ApiRunInSessionRequest =
  ExecdPaths["/session/{sessionId}/run"]["post"]["requestBody"]["content"]["application/json"];

interface StreamingExecutionSpec<TBody> {
  pathname: string;
  body: TBody;
  fallbackErrorMessage: string;
}

function toRunCommandRequest(command: string, opts?: RunCommandOpts): ApiRunCommandRequest {
  if (opts?.gid != null && opts.uid == null) {
    throw new Error("uid is required when gid is provided");
  }

  const body: ApiRunCommandRequest = {
    command,
    cwd: opts?.workingDirectory,
    background: !!opts?.background,
  };
  if (opts?.timeoutSeconds != null) {
    body.timeout = Math.round(opts.timeoutSeconds * 1000);
  }
  if (opts?.uid != null) {
    body.uid = opts.uid;
  }
  if (opts?.gid != null) {
    body.gid = opts.gid;
  }
  if (opts?.envs != null) {
    body.envs = opts.envs;
  }
  return body;
}

function toRunInSessionRequest(
  command: string,
  opts?: { workingDirectory?: string; timeoutSeconds?: number },
): ApiRunInSessionRequest {
  const body: ApiRunInSessionRequest = {
    command,
  };
  if (opts?.workingDirectory != null) {
    body.cwd = opts.workingDirectory;
  }
  if (opts?.timeoutSeconds != null) {
    body.timeout = Math.round(opts.timeoutSeconds * 1000);
  }
  return body;
}

function inferForegroundExitCode(execution: CommandExecution): number | null {
  const errorValue = execution.error?.value?.trim();
  const parsedExitCode =
    errorValue && /^-?\d+$/.test(errorValue) ? Number(errorValue) : Number.NaN;
  return execution.error != null
    ? (Number.isFinite(parsedExitCode) ? parsedExitCode : null)
    : execution.complete
      ? 0
      : null;
}

function assertNonBlank(value: string, field: string): void {
  if (!value.trim()) {
    throw new Error(`${field} cannot be empty`);
  }
}

function parseOptionalDate(value: unknown, field: string): Date | undefined {
  if (value == null) return undefined;
  if (value instanceof Date) return value;
  if (typeof value !== "string") {
    throw new Error(`Invalid ${field}: expected ISO string, got ${typeof value}`);
  }
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    throw new Error(`Invalid ${field}: ${value}`);
  }
  return parsed;
}

export interface CommandsAdapterOptions {
  /**
   * Must match the baseUrl used by the ExecdClient.
   */
  baseUrl: string;
  fetch?: typeof fetch;
  headers?: Record<string, string>;
}

export class CommandsAdapter implements ExecdCommands {
  private readonly fetch: typeof fetch;

  constructor(
    private readonly client: ExecdClient,
    private readonly opts: CommandsAdapterOptions,
  ) {
    this.fetch = opts.fetch ?? fetch;
  }

  private buildRunStreamSpec(
    command: string,
    opts?: RunCommandOpts,
  ): StreamingExecutionSpec<ApiRunCommandRequest> {
    assertNonBlank(command, "command");
    return {
      pathname: "/command",
      body: toRunCommandRequest(command, opts),
      fallbackErrorMessage: "Run command failed",
    };
  }

  private buildRunInSessionStreamSpec(
    sessionId: string,
    command: string,
    opts?: { workingDirectory?: string; timeoutSeconds?: number },
  ): StreamingExecutionSpec<ApiRunInSessionRequest> {
    assertNonBlank(sessionId, "sessionId");
    assertNonBlank(command, "command");
    return {
      pathname: `/session/${encodeURIComponent(sessionId)}/run`,
      body: toRunInSessionRequest(command, opts),
      fallbackErrorMessage: "Run in session failed",
    };
  }

  private async *streamExecution<TBody>(
    spec: StreamingExecutionSpec<TBody>,
    signal?: AbortSignal,
  ): AsyncIterable<ServerStreamEvent> {
    const url = joinUrl(this.opts.baseUrl, spec.pathname);
    const res = await this.fetch(url, {
      method: "POST",
      headers: {
        accept: "text/event-stream",
        "content-type": "application/json",
        ...(this.opts.headers ?? {}),
      },
      body: JSON.stringify(spec.body),
      signal,
    });

    for await (const ev of parseJsonEventStream<ServerStreamEvent>(res, {
      fallbackErrorMessage: spec.fallbackErrorMessage,
    })) {
      yield ev;
    }
  }

  private async consumeExecutionStream(
    stream: AsyncIterable<ServerStreamEvent>,
    handlers?: ExecutionHandlers,
    inferExitCode = false,
  ): Promise<CommandExecution> {
    const execution: CommandExecution = {
      logs: { stdout: [], stderr: [] },
      result: [],
    };
    const dispatcher = new ExecutionEventDispatcher(execution, handlers);
    for await (const ev of stream) {
      if (ev.type === "init" && (ev.text ?? "") === "" && execution.id) {
        (ev as { text?: string }).text = execution.id;
      }
      await dispatcher.dispatch(ev as any);
    }

    if (inferExitCode) {
      execution.exitCode = inferForegroundExitCode(execution);
    }

    return execution;
  }

  async interrupt(sessionId: string): Promise<void> {
    const { error, response } = await this.client.DELETE("/command", {
      params: { query: { id: sessionId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Interrupt command failed");
  }

  async getCommandStatus(commandId: string): Promise<CommandStatus> {
    const { data, error, response } = await this.client.GET("/command/status/{id}", {
      params: { path: { id: commandId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get command status failed");
    const ok = data as ApiCommandStatusOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Get command status failed: unexpected response shape");
    }
    return {
      id: ok.id,
      content: ok.content,
      running: ok.running,
      exitCode: ok.exit_code ?? null,
      error: ok.error,
      startedAt: parseOptionalDate(ok.started_at, "startedAt"),
      finishedAt: parseOptionalDate(ok.finished_at, "finishedAt") ?? null,
    };
  }

  async getBackgroundCommandLogs(commandId: string, cursor?: number): Promise<CommandLogs> {
    const { data, error, response } = await this.client.GET("/command/{id}/logs", {
      params: { path: { id: commandId }, query: cursor == null ? {} : { cursor } },
      parseAs: "text",
    });
    throwOnOpenApiFetchError({ error, response }, "Get command logs failed");
    const ok = data as ApiCommandLogsOk | undefined;
    if (typeof ok !== "string") {
      throw new Error("Get command logs failed: unexpected response shape");
    }
    const cursorHeader = response.headers.get("EXECD-COMMANDS-TAIL-CURSOR");
    const parsedCursor = (cursorHeader != null && cursorHeader !== "") ? Number(cursorHeader) : undefined;
    return {
      content: ok,
      cursor: Number.isFinite(parsedCursor ?? NaN) ? parsedCursor : undefined,
    };
  }

  async *runStream(
    command: string,
    opts?: RunCommandOpts,
    signal?: AbortSignal,
  ): AsyncIterable<ServerStreamEvent> {
    for await (const ev of this.streamExecution(
      this.buildRunStreamSpec(command, opts),
      signal,
    )) {
      yield ev;
    }
  }

  async run(
    command: string,
    opts?: RunCommandOpts,
    handlers?: ExecutionHandlers,
    signal?: AbortSignal,
  ): Promise<CommandExecution> {
    return this.consumeExecutionStream(
      this.runStream(command, opts, signal),
      handlers,
      !opts?.background,
    );
  }

  async createSession(options?: { workingDirectory?: string }): Promise<string> {
    const body: ApiCreateSessionRequest =
      options?.workingDirectory != null ? { cwd: options.workingDirectory } : {};
    const { data, error, response } = await this.client.POST("/session", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Create session failed");
    const ok = data as ApiCreateSessionOk | undefined;
    if (!ok || typeof (ok as { session_id?: string }).session_id !== "string") {
      throw new Error("Create session failed: unexpected response shape");
    }
    return (ok as { session_id: string }).session_id;
  }

  async *runInSessionStream(
    sessionId: string,
    command: string,
    opts?: { workingDirectory?: string; timeoutSeconds?: number },
    signal?: AbortSignal,
  ): AsyncIterable<ServerStreamEvent> {
    for await (const ev of this.streamExecution(
      this.buildRunInSessionStreamSpec(sessionId, command, opts),
      signal,
    )) {
      yield ev;
    }
  }

  async runInSession(
    sessionId: string,
    command: string,
    options?: { workingDirectory?: string; timeoutSeconds?: number },
    handlers?: ExecutionHandlers,
    signal?: AbortSignal,
  ): Promise<CommandExecution> {
    return this.consumeExecutionStream(
      this.runInSessionStream(sessionId, command, options, signal),
      handlers,
      true,
    );
  }

  async deleteSession(sessionId: string): Promise<void> {
    const { error, response } = await this.client.DELETE(
      "/session/{sessionId}",
      { params: { path: { sessionId } } },
    );
    throwOnOpenApiFetchError({ error, response }, "Delete session failed");
  }
}
