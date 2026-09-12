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
  ExecutionInstance,
  ExecutionOperation,
  CommandLogs,
  CommandStatus,
  RunCommandOpts,
  ServerStreamEvent,
} from "../models/execd.js";
import type { ExecdCommands, ExecutionOperations } from "../services/execdCommands.js";
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

function toRunCommandRequest(command: string | string[], opts?: RunCommandOpts): ApiRunCommandRequest {
  if (opts?.gid != null && opts.uid == null) {
    throw new Error("uid is required when gid is provided");
  }

  const body: ApiRunCommandRequest = {
    ...(typeof command === "string" ? { command } : { argv: command }),
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

export class CommandsAdapter implements ExecdCommands, ExecutionOperations {
  private readonly fetch: typeof fetch;
  private instanceCache?: { started: number; value: ExecutionInstance };
  private instanceFetch?: { started: number; promise: Promise<ExecutionInstance> };

  constructor(
    private readonly client: ExecdClient,
    private readonly opts: CommandsAdapterOptions,
  ) {
    this.fetch = opts.fetch ?? fetch;
  }

  async getExecutionInstance(): Promise<ExecutionInstance> {
    if (this.instanceCache && performance.now() - this.instanceCache.started < 60_000) {
      return { ...this.instanceCache.value };
    }
    const pending = this.instanceFetch ??= {
      started: performance.now(),
      promise: this.fetchExecutionInstance(),
    };
    try {
      const value = await pending.promise;
      if (this.instanceFetch === pending) {
        this.instanceCache = { started: pending.started, value: { ...value } };
      }
      return { ...value };
    } finally {
      if (this.instanceFetch === pending) this.instanceFetch = undefined;
    }
  }

  private async fetchExecutionInstance(): Promise<ExecutionInstance> {
    const { data, error, response } = await this.client.GET("/execution/instance");
    throwOnOpenApiFetchError({ error, response }, "Get execution instance failed");
    if (!data) throw new Error("Missing execution instance");
    return data;
  }

  private invalidateOperationInstance(error: unknown): void {
    if (error && typeof error === "object" && "code" in error &&
      (error.code === "operation_instance_mismatch" || error.code === "operation_expired")) {
      this.instanceCache = undefined;
      this.instanceFetch = undefined;
    }
  }

  async getExecutionOperation(kind: "command" | "pty", operationId: string): Promise<ExecutionOperation> {
    const { data, error, response } = await this.client.GET("/execution/operation", {
      params: { query: { kind }, header: { "X-EXECD-OPERATION-ID": operationId } },
    });
    this.invalidateOperationInstance(error);
    throwOnOpenApiFetchError({ error, response }, "Get execution operation failed");
    if (!data) throw new Error("Missing execution operation");
    return data;
  }

  async createCommandOperation(operationId: string, command: string, opts?: RunCommandOpts): Promise<ExecutionOperation> {
    assertNonBlank(operationId, "operationId");
    const { data, error, response } = await this.client.POST("/command/operations", {
      body: { ...toRunCommandRequest(command, opts), operation_id: operationId },
    });
    this.invalidateOperationInstance(error);
    throwOnOpenApiFetchError({ error, response }, "Create command operation failed");
    if (!data || !("state" in data)) throw new Error("Missing execution operation");
    return data;
  }

  async createPTYOperation(operationId: string, opts?: { cwd?: string; command?: string }): Promise<ExecutionOperation> {
    assertNonBlank(operationId, "operationId");
    const { data, error, response } = await this.client.POST("/pty/operations", {
      body: { ...opts, operation_id: operationId },
    });
    this.invalidateOperationInstance(error);
    throwOnOpenApiFetchError({ error, response }, "Create PTY operation failed");
    if (!data || !("state" in data)) throw new Error("Missing execution operation");
    return data;
  }

  private buildRunStreamSpec(
    command: string | string[],
    opts?: RunCommandOpts,
  ): StreamingExecutionSpec<ApiRunCommandRequest> {
    if (typeof command === "string") {
      assertNonBlank(command, "command");
    } else {
      if (!Array.isArray(command) || !command.length || !command[0]) {
        throw new Error("argv requires a non-empty executable and strings without NUL");
      }
      for (const arg of command) {
        if (typeof arg !== "string" || arg.includes("\0")) {
          throw new Error("argv requires a non-empty executable and strings without NUL");
        }
      }
    }
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
    isBackground = false,
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
      if (isBackground && ev.type === "execution_complete") {
        // Background commands are done once execution_complete arrives; do
        // not wait for the chunked terminator, which execd sends only after
        // a graceful-shutdown sleep and can be lost if the connection is
        // closed early (#1528).
        break;
      }
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

    let content: string;
    if (typeof data === "string") {
      content = data;
    } else if (data == null && response.ok) {
      content = "";
    } else {
      throw new Error("Get command logs failed: unexpected response shape");
    }

    const cursorHeader = response.headers.get("EXECD-COMMANDS-TAIL-CURSOR");
    const parsedCursor = cursorHeader != null && cursorHeader !== "" ? Number(cursorHeader) : undefined;
    return {
      content,
      cursor: Number.isFinite(parsedCursor ?? NaN) ? parsedCursor : undefined,
    };
  }

  async *runStream(
    command: string | string[],
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
    command: string | string[],
    opts?: RunCommandOpts,
    handlers?: ExecutionHandlers,
    signal?: AbortSignal,
  ): Promise<CommandExecution> {
    return this.consumeExecutionStream(
      this.runStream(command, opts, signal),
      handlers,
      !opts?.background,
      !!opts?.background,
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
