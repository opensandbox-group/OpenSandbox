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
}
