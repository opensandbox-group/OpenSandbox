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
  CommandSummary,
  ListCommandsOptions,
  ListCommandsPage,
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

const runningFields = new Set(["session", "running", "background", "started_at"]);
const terminalFields = new Set([
  "session",
  "running",
  "background",
  "started_at",
  "finished_at",
  "exit_code",
  "error",
]);
const rfc3339Pattern =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(Z|[+-](\d{2}):(\d{2}))$/i;

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function requireExactKeys(item: Record<string, unknown>, allowed: Set<string>, branch: string): void {
  for (const key of Object.keys(item)) {
    if (!allowed.has(key)) throw new Error(`${branch} command has unknown field: ${key}`);
  }
}

function requireInt32OrNull(value: unknown, field: string): number | null {
  if (value === null) return null;
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < -2147483648 ||
    value > 2147483647
  ) {
    throw new Error(`Invalid ${field}`);
  }
  return value;
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

function parseRequiredRfc3339Date(value: unknown, field: string): Date {
  if (value == null) throw new Error(`Missing ${field}`);
  return parseRfc3339Date(value, field);
}

function daysInMonth(year: number, month: number): number {
  if (month === 2) {
    return year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0) ? 29 : 28;
  }
  return [4, 6, 9, 11].includes(month) ? 30 : 31;
}

function parseRfc3339Date(value: unknown, field: string): Date {
  if (typeof value !== "string") throw new Error(`Invalid ${field}`);
  const match = rfc3339Pattern.exec(value);
  if (!match) throw new Error(`Invalid ${field}: ${value}`);

  const [, year, month, day, hour, minute, second, fraction, timezone, offsetHour, offsetMinute] = match;
  const numericYear = Number(year);
  const numericMonth = Number(month);
  const numericDay = Number(day);
  const numericHour = Number(hour);
  const numericMinute = Number(minute);
  const numericSecond = Number(second);
  const numericOffsetHour = offsetHour == null ? 0 : Number(offsetHour);
  const numericOffsetMinute = offsetMinute == null ? 0 : Number(offsetMinute);
  if (
    numericMonth < 1 ||
    numericMonth > 12 ||
    numericDay < 1 ||
    numericDay > daysInMonth(numericYear, numericMonth) ||
    numericHour > 23 ||
    numericMinute > 59 ||
    numericSecond > 59 ||
    numericOffsetHour > 23 ||
    numericOffsetMinute > 59
  ) {
    throw new Error(`Invalid ${field}: ${value}`);
  }

  const milliseconds = fraction == null ? 0 : Number(`${fraction.slice(0, 3).padEnd(3, "0")}`);
  const offset =
    timezone.toUpperCase() === "Z"
      ? 0
      : (numericOffsetHour * 60 + numericOffsetMinute) * (timezone.startsWith("+") ? 1 : -1);
  const parsed = new Date(0);
  parsed.setUTCFullYear(numericYear, numericMonth - 1, numericDay);
  parsed.setUTCHours(numericHour, numericMinute, numericSecond, milliseconds);
  parsed.setTime(parsed.getTime() - offset * 60_000);
  if (Number.isNaN(parsed.getTime())) throw new Error(`Invalid ${field}: ${value}`);
  return parsed;
}

function requireString(item: Record<string, unknown>, field: string): string {
  if (!Object.hasOwn(item, field) || typeof item[field] !== "string") {
    throw new Error(`Invalid ${field}`);
  }
  return item[field];
}

function toCommandSummary(value: unknown): CommandSummary {
  if (!isRecord(value)) throw new Error("Invalid command summary");
  if (!Object.hasOwn(value, "running") || typeof value.running !== "boolean") {
    throw new Error("Invalid command summary");
  }

  const branch = value.running ? "running" : "terminal";
  requireExactKeys(value, value.running ? runningFields : terminalFields, branch);
  const base = {
    session: requireString(value, "session"),
    background: (() => {
      if (!Object.hasOwn(value, "background") || typeof value.background !== "boolean") {
        throw new Error("Invalid background");
      }
      return value.background;
    })(),
    startedAt: (() => {
      if (!Object.hasOwn(value, "started_at")) throw new Error("Missing started_at");
      return parseRequiredRfc3339Date(value.started_at, "started_at");
    })(),
  };

  if (value.running) return { ...base, running: true };
  if (!Object.hasOwn(value, "finished_at") || !Object.hasOwn(value, "exit_code")) {
    throw new Error("Invalid terminal command summary");
  }
  const error = value.error;
  if (Object.hasOwn(value, "error") && typeof error !== "string") {
    throw new Error("Invalid error");
  }
  return {
    ...base,
    running: false,
    finishedAt: parseRequiredRfc3339Date(value.finished_at, "finished_at"),
    exitCode: requireInt32OrNull(value.exit_code, "exit_code"),
    ...(typeof error === "string" ? { error } : {}),
  };
}

function parseListCommandsPage(value: unknown): ListCommandsPage {
  if (
    !isRecord(value) ||
    !Object.hasOwn(value, "commands") ||
    !Array.isArray(value.commands) ||
    !Object.hasOwn(value, "pagination") ||
    !isRecord(value.pagination)
  ) {
    throw new Error("List commands failed: unexpected response shape");
  }
  const { pagination } = value;
  if (
    !Object.hasOwn(pagination, "limit") ||
    typeof pagination.limit !== "number" ||
    !Number.isInteger(pagination.limit) ||
    pagination.limit < 1 ||
    pagination.limit > 100
  ) {
    throw new Error("Invalid pagination limit");
  }
  const nextCursor = pagination.nextCursor;
  if (Object.hasOwn(pagination, "nextCursor") && typeof nextCursor !== "string") {
    throw new Error("Invalid nextCursor");
  }
  if (typeof nextCursor === "string" && nextCursor.trim() === "") {
    throw new Error("Invalid nextCursor");
  }
  return {
    commands: value.commands.map(toCommandSummary),
    pagination: {
      limit: pagination.limit,
      ...(typeof nextCursor === "string" ? { nextCursor } : {}),
    },
  };
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

  async listCommands(options?: ListCommandsOptions): Promise<ListCommandsPage> {
    const query: Record<string, boolean | number | string> = {};
    if (options?.running != null) query.running = options.running;
    if (options?.limit != null) query.limit = options.limit;
    if (options?.cursor != null && options.cursor.trim() !== "") {
      query.cursor = options.cursor;
    }
    const { data, error, response } = await this.client.GET("/command", {
      params: { query },
    });
    throwOnOpenApiFetchError({ error, response }, "List commands failed");
    return parseListCommandsPage(data);
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
