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

import type { Execution } from "./execution.js";

/**
 * Domain models for execd interactions.
 *
 * IMPORTANT:
 * - These are NOT OpenAPI-generated types.
 * - They are intentionally stable and JS-friendly.
 */
export interface ServerStreamEvent extends Record<string, unknown> {
  type:
    | "init"
    | "stdout"
    | "stderr"
    | "result"
    | "execution_count"
    | "execution_complete"
    | "error"
    | string;
  timestamp?: number;
  text?: string;
  results?: Record<string, unknown>;
  error?: Record<string, unknown>;
  /** Monotonic event ID for SSE resume. */
  eid?: number;
}

export interface CodeContextRequest extends Record<string, unknown> {
  language: string;
}

export type SupportedLanguage =
  | "python"
  | "go"
  | "javascript"
  | "typescript"
  | "bash"
  | "java";

export interface RunCommandOpts {
  /**
   * Working directory for command execution (maps to API `cwd`).
   */
  workingDirectory?: string;
  /**
   * Run command in detached mode.
   */
  background?: boolean;
  /**
   * Maximum execution time in seconds; server will terminate the command when reached.
   * If omitted, the server will not enforce any timeout.
   */
  timeoutSeconds?: number;
  /**
   * Unix user ID used to run the command process.
   */
  uid?: number;
  /**
   * Unix group ID used to run the command process. Requires `uid`.
   */
  gid?: number;
  /**
   * Environment variables injected into the command process.
   */
  envs?: Record<string, string>;
}

export interface CommandStatus {
  id?: string;
  content?: string;
  running?: boolean;
  exitCode?: number | null;
  error?: string;
  startedAt?: Date;
  finishedAt?: Date | null;
}

export interface CommandLogs {
  content: string;
  cursor?: number;
}

export type CommandExecution = Execution;

export interface Metrics extends Record<string, unknown> {
  cpu_count?: number;
  cpu_used_pct?: number;
  mem_total_mib?: number;
  mem_used_mib?: number;
  timestamp?: number;
}

/**
 * Normalized, JS-friendly metrics.
 */
export interface SandboxMetrics {
  cpuCount: number;
  cpuUsedPercentage: number;
  memoryTotalMiB: number;
  memoryUsedMiB: number;
  timestamp: number;
}

export type PingResponse = Record<string, unknown>;