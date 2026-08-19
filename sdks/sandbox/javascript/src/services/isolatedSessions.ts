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

import type { CommandExecution } from "../models/execd.js";
import type { ExecutionHandlers } from "../models/execution.js";
import type { SandboxFiles } from "./filesystem.js";
import type {
  BindMount,
  CreateIsolatedSessionRequest,
  IsolatedBackgroundRun,
  IsolatedCapabilities,
  IsolatedRunLogs,
  IsolatedRunOpts,
  IsolatedRunStatus,
  IsolatedSessionInfo,
  IsolatedSessionState,
  IsolatedSessionSummary,
} from "../models/isolated.js";

export interface IsolationSession {
  readonly sessionId: string;
  readonly info: IsolatedSessionInfo;
  readonly files: SandboxFiles;
  run(
    code: string,
    opts?: IsolatedRunOpts,
    handlers?: ExecutionHandlers,
    signal?: AbortSignal,
  ): Promise<CommandExecution>;
  /**
   * Start `code` detached inside the session and return a run handle.
   *
   * The run's combined output and exit code are captured by execd; poll
   * them with {@link getRunStatus} and {@link getRunLogs}. The run is not
   * time-limited and idle GC is suspended while it is active. Background
   * runs require a writable log location, so sessions with a read-only
   * (`ro`) workspace reject them.
   */
  runBackground(code: string, opts?: IsolatedRunOpts): Promise<IsolatedBackgroundRun>;
  /** Return the lifecycle state of a background run. */
  getRunStatus(runId: string): Promise<IsolatedRunStatus>;
  /**
   * Return the background run's log from `cursor` plus the next cursor.
   *
   * Each call returns at most 16 MiB; pass the returned `cursor` to fetch
   * the remainder. Per-run log retention is capped at 16 MiB (output beyond
   * it is discarded when the run finishes), so drain incrementally while
   * the run is active if more than one page is needed.
   */
  getRunLogs(runId: string, cursor?: number): Promise<IsolatedRunLogs>;
  get(): Promise<IsolatedSessionState>;
  delete(): Promise<void>;
}

export interface RunOnceOpts {
  workspaceMode?: "rw" | "overlay" | "ro";
  runOpts?: IsolatedRunOpts;
  handlers?: ExecutionHandlers;
  profile?: "strict" | "balanced";
  shareNet?: boolean;
  binds?: BindMount[];
  signal?: AbortSignal;
}

export interface IsolationService {
  create(request: CreateIsolatedSessionRequest): Promise<IsolationSession>;
  /**
   * Rebuild a session handle for an already-existing isolated session by id.
   *
   * Useful for stateless callers (e.g. a serverless worker restarted
   * mid-flight) that only retain the session id. Issues a GET against
   * `/v1/isolated/session/{id}` and echoes any creation parameters the
   * execd side returns into `handle.info`. Older execd builds may omit the
   * creation-parameter fields; those info fields will be undefined but
   * `run`, `get`, `delete`, and `files` still work because they only need
   * the session id.
   *
   * A missing session (404) is surfaced as the SDK's standard request
   * failure error, matching what `session.get()` does today.
   */
  attach(sessionId: string): Promise<IsolationSession>;
  capabilities(): Promise<IsolatedCapabilities>;
  list(): Promise<IsolatedSessionSummary[]>;
  /**
   * Create a session, run `code`, and delete the session (auto-cleanup).
   * Cleanup is best-effort and never masks the original error.
   */
  runOnce(
    code: string,
    workspace: string,
    opts?: RunOnceOpts,
  ): Promise<CommandExecution>;
  /**
   * Create a session, invoke `fn`, and delete the session on exit
   * regardless of whether `fn` throws.
   */
  withSession<T>(
    request: CreateIsolatedSessionRequest,
    fn: (session: IsolationSession) => Promise<T>,
  ): Promise<T>;
}
