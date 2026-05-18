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

import type { Execution, ExecutionComplete, ExecutionError, ExecutionHandlers, ExecutionInit, ExecutionResult, OutputMessage } from "./execution.js";
import type { ServerStreamEvent } from "./execd.js";

function extractText(results: ServerStreamEvent["results"] | undefined): string | undefined {
  if (!results || typeof results !== "object") return undefined;
  const r = results as any;
  const v = r["text/plain"] ?? r.text ?? r.textPlain;
  return v == null ? undefined : String(v);
}

/**
 * Dispatches streamed execution events to handlers.
 *
 * This mutates the provided `execution` object (appending logs/results and setting fields like
 * `id`, `executionCount`, and `complete`) and invokes optional callbacks in {@link ExecutionHandlers}.
 */
export class ExecutionEventDispatcher {
  constructor(
    private readonly execution: Execution,
    private readonly handlers?: ExecutionHandlers,
  ) {}

  async dispatch(ev: ServerStreamEvent): Promise<void> {
    if (ev.eid != null && ev.eid > (this.execution.lastEid ?? 0)) {
      this.execution.lastEid = ev.eid;
    }

    await this.handlers?.onEvent?.(ev);

    const ts = ev.timestamp ?? Date.now();
    switch (ev.type) {
      case "init": {
        const id = ev.text ?? "";
        if (id) this.execution.id = id;
        const init: ExecutionInit = { id, timestamp: ts };
        await this.handlers?.onInit?.(init);
        return;
      }
      case "stdout": {
        const msg: OutputMessage = { text: ev.text ?? "", timestamp: ts, isError: false };
        this.execution.logs.stdout.push(msg);
        await this.handlers?.onStdout?.(msg);
        return;
      }
      case "stderr": {
        const msg: OutputMessage = { text: ev.text ?? "", timestamp: ts, isError: true };
        this.execution.logs.stderr.push(msg);
        await this.handlers?.onStderr?.(msg);
        return;
      }
      case "result": {
        const r: ExecutionResult = { text: extractText(ev.results), timestamp: ts, raw: ev.results as any };
        this.execution.result.push(r);
        await this.handlers?.onResult?.(r);
        return;
      }
      case "execution_count": {
        const c = (ev as any).execution_count;
        if (typeof c === "number") this.execution.executionCount = c;
        return;
      }
      case "execution_complete": {
        const ms = (ev as any).execution_time;
        const complete: ExecutionComplete = { timestamp: ts, executionTimeMs: typeof ms === "number" ? ms : 0 };
        this.execution.complete = complete;
        await this.handlers?.onExecutionComplete?.(complete);
        return;
      }
      case "error": {
        const e = ev.error as any;
        if (e) {
          const err: ExecutionError = {
            name: String(e.ename ?? e.name ?? ""),
            value: String(e.evalue ?? e.value ?? ""),
            timestamp: ts,
            traceback: Array.isArray(e.traceback) ? e.traceback.map(String) : [],
          };
          this.execution.error = err;
          await this.handlers?.onError?.(err);
        }
        return;
      }
      default:
        return;
    }
  }
}