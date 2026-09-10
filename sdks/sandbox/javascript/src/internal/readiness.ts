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

import { SandboxApiException, SandboxReadyTimeoutException } from "../core/exceptions.js";

export class ReadinessBudget {
  private readonly deadline: number;
  private lastError: unknown;
  private context?: string;
  private attempts = 0;
  private timedOut = false;

  constructor(private readonly seconds: number, private readonly caller?: AbortSignal) {
    this.deadline = performance.now() + seconds * 1000;
  }

  remaining(): number {
    this.caller?.throwIfAborted();
    const remaining = this.deadline - performance.now();
    if (this.timedOut || remaining <= 0) throw this.timeout();
    return remaining;
  }

  healthContext(context: string): void {
    this.context = context;
    this.lastError = undefined;
  }
  attempt(): void { this.attempts++; }

  record(error: unknown): void { this.lastError = error; }

  private timeout(): SandboxReadyTimeoutException {
    return new SandboxReadyTimeoutException({
      message: this.context === undefined
        ? `Sandbox readiness timed out. Last error: ${String(this.lastError ?? "Endpoint not yet available.")}`
        : `Sandbox health check timed out after ${this.seconds}s (${this.attempts} attempts). ${this.lastError instanceof Error ? `Last health check error: ${this.lastError.message}` : String(this.lastError ?? "Health check returned false continuously.")} Connection context: ${this.context}.`,
      cause: this.lastError,
    });
  }

  async run<T>(action: (signal: AbortSignal) => Promise<T>): Promise<T> {
    const remaining = this.remaining();
    const controller = new AbortController();
    const onAbort = () => controller.abort(this.caller?.reason);
    this.caller?.addEventListener("abort", onAbort, { once: true });
    const timer = setTimeout(() => {
      this.timedOut = true;
      controller.abort(this.timeout());
    }, remaining);
    let rejectAbort: (() => void) | undefined;
    try {
      const aborted = new Promise<never>((_, reject) => {
        rejectAbort = () => reject(controller.signal.reason);
        controller.signal.addEventListener("abort", rejectAbort, { once: true });
      });
      const result = await Promise.race([action(controller.signal), aborted]);
      this.remaining();
      return result;
    } catch (error) {
      this.remaining();
      throw error;
    } finally {
      clearTimeout(timer);
      this.caller?.removeEventListener("abort", onAbort);
      if (rejectAbort) controller.signal.removeEventListener("abort", rejectAbort);
    }
  }

  async pause(interval: number): Promise<void> {
    const duration = Math.min(interval, this.remaining());
    await new Promise<void>((resolve, reject) => {
      const timer = setTimeout(() => { this.caller?.removeEventListener("abort", abort); resolve(); }, duration);
      const abort = () => { clearTimeout(timer); reject(this.caller?.reason); };
      this.caller?.addEventListener("abort", abort, { once: true });
    });
    this.remaining();
  }

  async endpoint<T>(action: (signal: AbortSignal) => Promise<T>, interval: number): Promise<T> {
    while (true) {
      try { return await this.run(action); }
      catch (error) {
        if (!(error instanceof SandboxApiException) || error.statusCode !== 404 ||
            error.error.code !== "KUBERNETES::POD_IP_NOT_AVAILABLE") throw error;
        this.record(error);
      }
      await this.pause(interval);
    }
  }
}
