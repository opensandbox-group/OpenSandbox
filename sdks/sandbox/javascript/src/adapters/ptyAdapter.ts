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
import type { PtySession, PtySessionStatus } from "../models/execd.js";
import type { ExecdPty } from "../services/execdPty.js";
import { SandboxError, SandboxException } from "../core/exceptions.js";

export class PtyAdapter implements ExecdPty {
  constructor(private readonly client: ExecdClient) {}

  async createSession(opts?: { cwd?: string; command?: string }): Promise<PtySession> {
    const { data, error, response } = await this.client.POST("/pty", {
      body: { cwd: opts?.cwd, command: opts?.command },
    });
    throwOnOpenApiFetchError({ error, response }, "Create PTY session failed");
    return { sessionId: data!.session_id };
  }

  async getSession(sessionId: string): Promise<PtySessionStatus> {
    const { data, error, response } = await this.client.GET("/pty/{sessionId}", {
      params: { path: { sessionId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get PTY session failed");
    return {
      sessionId: data!.session_id,
      running: data!.running,
      outputOffset: data!.output_offset,
    };
  }

  async deleteSession(sessionId: string): Promise<void> {
    // Success is an empty 200 body (Content-Length: 0), which openapi-fetch skips
    // parsing; error responses (e.g. 404 CONTEXT_NOT_FOUND, 501 NOT_SUPPORTED) keep
    // their JSON code/message so throwOnOpenApiFetchError can surface them.
    const { error, response } = await this.client.DELETE("/pty/{sessionId}", {
      params: { path: { sessionId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Delete PTY session failed");
  }
}

/**
 * Fallback PTY service used when a custom {@link AdapterFactory} does not supply a
 * PTY adapter. Keeps `sandbox.pty` defined while failing loudly on use, so the
 * execd stack contract stays additive for pre-existing factories.
 */
export class UnavailablePtyAdapter implements ExecdPty {
  private failure(): SandboxException {
    return new SandboxException({
      message:
        "PTY service is not available: the configured adapter factory did not provide a PTY adapter.",
      error: new SandboxError(SandboxError.INVALID_ARGUMENT, "PTY service unavailable"),
    });
  }

  createSession(): Promise<PtySession> {
    return Promise.reject(this.failure());
  }

  getSession(): Promise<PtySessionStatus> {
    return Promise.reject(this.failure());
  }

  deleteSession(): Promise<void> {
    return Promise.reject(this.failure());
  }
}
