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

export interface OutputMessage {
  text: string;
  timestamp: number;
  isError?: boolean;
}

export interface ExecutionResult {
  text?: string;
  timestamp: number;
  /**
   * Raw mime map from execd event (e.g. "text/plain", "text/html", ...)
   */
  raw?: Record<string, unknown>;
}

export interface ExecutionError {
  name: string;
  value: string;
  timestamp: number;
  traceback: string[];
  /**
   * Exit status reported by execd for a `CommandExecError` when the command ran and finished
   * with a non-zero status (-1 when it was terminated by a signal). Absent when the process
   * failed to start, for non-command errors, and on servers that predate `error.exit_code`.
   */
  exitCode?: number;
}

export interface ExecutionComplete {
  timestamp: number;
  executionTimeMs: number;
}

export interface ExecutionInit {
  id: string;
  timestamp: number;
}

export interface Execution {
  id?: string;
  executionCount?: number;
  logs: {
    stdout: OutputMessage[];
    stderr: OutputMessage[];
  };
  result: ExecutionResult[];
  error?: ExecutionError;
  complete?: ExecutionComplete;
  exitCode?: number | null;
}

/**
 * Normalizes a wire `error.exit_code` value. Only integers are accepted; anything else
 * (missing, non-numeric, fractional) yields `undefined` so callers fall back to `evalue`.
 *
 * @internal
 */
export function normalizeExitCode(value: unknown): number | undefined {
  return typeof value === "number" && Number.isInteger(value) ? value : undefined;
}

/**
 * Infers the final exit code of a foreground command execution.
 *
 * Prefers the authoritative `error.exitCode` (from `error.exit_code`) and falls back to
 * parsing `error.value` (`evalue`) for servers that predate `exit_code`. A completed
 * execution without an error is `0`; anything else is `null`.
 *
 * @internal
 */
export function inferExecutionExitCode(execution: Pick<Execution, "error" | "complete">): number | null {
  const error = execution.error;
  if (error != null) {
    const exitCode = normalizeExitCode(error.exitCode);
    if (exitCode !== undefined) {
      return exitCode;
    }
    const errorValue = error.value?.trim();
    const parsedExitCode =
      errorValue && /^-?\d+$/.test(errorValue) ? Number(errorValue) : Number.NaN;
    return Number.isFinite(parsedExitCode) ? parsedExitCode : null;
  }
  return execution.complete ? 0 : null;
}

export interface ExecutionHandlers {
  /**
   * Optional low-level hook for every server-sent event (SSE) received.
   * Kept as `unknown` to avoid coupling to a specific OpenAPI schema module.
   */
  onEvent?: (ev: unknown) => void | Promise<void>;
  onStdout?: (msg: OutputMessage) => void | Promise<void>;
  onStderr?: (msg: OutputMessage) => void | Promise<void>;
  onResult?: (res: ExecutionResult) => void | Promise<void>;
  onExecutionComplete?: (c: ExecutionComplete) => void | Promise<void>;
  onError?: (err: ExecutionError) => void | Promise<void>;
  onInit?: (init: ExecutionInit) => void | Promise<void>;
  /**
   * When true, stdout/stderr messages are only delivered to handlers without
   * being accumulated in the execution logs. Use for long-running executions
   * to prevent unbounded memory growth.
   */
  skipAccumulation?: boolean;
}
