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

import type { ExecutionHandlers } from "../models/execution.js";
import type {
  CommandExecution,
  CommandLogs,
  CommandStatus,
  RunCommandOpts,
  ServerStreamEvent,
} from "../models/execd.js";

export interface ExecdCommands {
  /**
   * Run a command and stream server events (SSE). This is the lowest-level API.
   */
  runStream(command: string | string[], opts?: RunCommandOpts, signal?: AbortSignal): AsyncIterable<ServerStreamEvent>;

  /**
   * Convenience: run a command, consume the stream, and build a structured execution result.
   */
  run(command: string | string[], opts?: RunCommandOpts, handlers?: ExecutionHandlers, signal?: AbortSignal): Promise<CommandExecution>;

  /**
   * Persist an environment variable for future commands and sessions.
   *
   * Appends `KEY=VALUE` to the sandbox env file that the runtime loads for every
   * command and session (the file pointed to by the sandbox's `EXECD_ENVS`
   * variable, resolved inside the sandbox). Keys must match
   * `[A-Za-z_][A-Za-z0-9_]*`; values are stored verbatim with proper escaping.
   * The file is append-only: when a key is written multiple times, the last
   * entry wins.
   *
   * @param key Environment variable name
   * @param value Environment variable value
   * @throws If arguments are invalid or the sandbox fails to persist the variable
   */
  setEnv(key: string, value: string): Promise<void>;

  /**
   * Interrupt the current execution in the given context/session.
   *
   * Note: Execd spec uses `DELETE /command?id=<sessionId>`.
   */
  interrupt(sessionId: string): Promise<void>;

  /**
   * Get the current running status for a command id.
   */
  getCommandStatus(commandId: string): Promise<CommandStatus>;

  /**
   * Get background command logs (non-streamed).
   */
  getBackgroundCommandLogs(commandId: string, cursor?: number): Promise<CommandLogs>;

  /**
   * Create a bash session with optional working directory.
   * Returns session ID for use with runInSession and deleteSession.
   */
  createSession(options?: { workingDirectory?: string }): Promise<string>;

  /**
   * Run a shell command in an existing bash session (SSE stream, same event shape as run).
   * Optional workingDirectory and timeout apply to this run only; session state (e.g. env) persists.
   */
  runInSession(
    sessionId: string,
    command: string,
    options?: {
      workingDirectory?: string;
      timeoutSeconds?: number;
    },
    handlers?: ExecutionHandlers,
    signal?: AbortSignal,
  ): Promise<CommandExecution>;

  /**
   * Delete a bash session by ID. Frees resources; session ID must have been returned by createSession.
   */
  deleteSession(sessionId: string): Promise<void>;
}
