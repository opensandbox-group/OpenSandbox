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

import type { DiagnosticClient } from "../openapi/diagnosticClient.js";
import type { DiagnosticContent, DiagnosticContext } from "../models/diagnostics.js";
import type { Diagnostics } from "../services/diagnostics.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";

export class DiagnosticsAdapter implements Diagnostics {
  constructor(private readonly client: DiagnosticClient) {}

  async getContext(sandboxId: string): Promise<DiagnosticContext> {
    const result = await this.client.GET("/sandboxes/{sandboxId}/diagnostics/context", {
      params: { path: { sandboxId } },
    });
    throwOnOpenApiFetchError(result, "Get diagnostic context failed");
    if (!result.data) throw new Error("Missing diagnostic context response");
    return result.data;
  }

  getLogs(sandboxId: string, scope: string): Promise<DiagnosticContent> {
    return this.getContent(sandboxId, scope, "logs");
  }

  getEvents(sandboxId: string, scope: string): Promise<DiagnosticContent> {
    return this.getContent(sandboxId, scope, "events");
  }

  private async getContent(
    sandboxId: string, scope: string, kind: "logs" | "events",
  ): Promise<DiagnosticContent> {
    const path = kind === "logs"
      ? "/sandboxes/{sandboxId}/diagnostics/logs"
      : "/sandboxes/{sandboxId}/diagnostics/events";
    const result = await this.client.GET(path, {
      params: { path: { sandboxId }, query: { scope } },
    });
    throwOnOpenApiFetchError(result, `Get diagnostic ${kind} failed`);
    const data = result.data;
    if (!data) throw new Error("Missing diagnostic content response");
    return { ...data, expiresAt: data.expiresAt == null ? undefined : new Date(data.expiresAt) };
  }
}

export function requireDiagnostics(service: Diagnostics | undefined): Diagnostics {
  if (!service) throw new Error("The adapter factory does not provide diagnostics");
  return service;
}
