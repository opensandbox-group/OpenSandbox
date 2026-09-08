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

import type { ExecdClient, ExecdPaths } from "@alibaba-group/opensandbox/internal";
import type { ServerStreamEvent } from "@alibaba-group/opensandbox";
import type { Execution, ExecutionHandlers } from "@alibaba-group/opensandbox";
import {
  ExecutionEventDispatcher,
  InvalidArgumentException,
} from "@alibaba-group/opensandbox";

import type { Codes } from "../services/codes.js";
import type { CodeContext, SupportedLanguage } from "../models.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import { parseJsonEventStream } from "./sse.js";

type ApiCreateContextRequest =
  ExecdPaths["/code/context"]["post"]["requestBody"]["content"]["application/json"];
type ApiCreateContextOk =
  ExecdPaths["/code/context"]["post"]["responses"][200]["content"]["application/json"];
type ApiGetContextOk =
  ExecdPaths["/code/contexts/{context_id}"]["get"]["responses"][200]["content"]["application/json"];
type ApiListContextsOk =
  ExecdPaths["/code/contexts"]["get"]["responses"][200]["content"]["application/json"];
type ApiRunCodeRequest =
  ExecdPaths["/code"]["post"]["requestBody"]["content"]["application/json"];

/**
 * Single-layer codes adapter for the Code Interpreter SDK.
 *
 * - Handles HTTP/SSE streaming via the underlying execd adapter
 * - Builds the structured {@link Execution} result for `run(...)`
 */
function joinUrl(baseUrl: string, pathname: string): string {
  const base = baseUrl.endsWith("/") ? baseUrl.slice(0, -1) : baseUrl;
  const path = pathname.startsWith("/") ? pathname : `/${pathname}`;
  return `${base}${path}`;
}

export class CodesAdapter implements Codes {
  private readonly fetch: typeof fetch;

  constructor(
    private readonly client: ExecdClient,
    private readonly opts: { baseUrl: string; fetch?: typeof fetch; headers?: Record<string, string> },
  ) {
    this.fetch = opts.fetch ?? fetch;
  }

  async createContext(language: SupportedLanguage): Promise<CodeContext> {
    const body: ApiCreateContextRequest = { language };
    const { data, error, response } = await this.client.POST("/code/context", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Create code context failed");
    const ok = data as ApiCreateContextOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Create code context failed: unexpected response shape");
    }
    if (typeof ok.language !== "string" || !ok.language) {
      throw new Error("Create code context failed: missing language");
    }
    return { id: ok.id, language: ok.language };
  }

  async getContext(contextId: string): Promise<CodeContext> {
    if (!contextId?.trim()) {
      throw new InvalidArgumentException({ message: "contextId cannot be empty" });
    }
    const { data, error, response } = await this.client.GET("/code/contexts/{context_id}", {
      params: { path: { context_id: contextId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get code context failed");
    const ok = data as ApiGetContextOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Get code context failed: unexpected response shape");
    }
    if (typeof (ok as any).language !== "string" || !(ok as any).language) {
      throw new Error("Get code context failed: missing language");
    }
    return { id: (ok as any).id, language: (ok as any).language };
  }

  async listContexts(language?: SupportedLanguage): Promise<CodeContext[]> {
    const { data, error, response } = await this.client.GET("/code/contexts", {
      params: language ? { query: { language } } : undefined,
    } as any);
    throwOnOpenApiFetchError({ error, response }, "List code contexts failed");
    const ok = data as ApiListContextsOk | undefined;
    if (!Array.isArray(ok)) {
      throw new Error("List code contexts failed: unexpected response shape");
    }
    return ok
      .filter((c) => c && typeof c === "object")
      .map((c: any) => ({ id: c.id, language: c.language as any }));
  }

  async deleteContext(contextId: string): Promise<void> {
    if (!contextId?.trim()) {
      throw new InvalidArgumentException({ message: "contextId cannot be empty" });
    }
    const { error, response } = await this.client.DELETE("/code/contexts/{context_id}", {
      params: { path: { context_id: contextId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Delete code context failed");
  }

  async deleteContexts(language: SupportedLanguage): Promise<void> {
    const { error, response } = await this.client.DELETE("/code/contexts", {
      params: { query: { language } },
    });
    throwOnOpenApiFetchError({ error, response }, "Delete code contexts failed");
  }

  async interrupt(contextId: string): Promise<void> {
    const { error, response } = await this.client.DELETE("/code", {
      params: { query: { id: contextId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Interrupt code failed");
  }

  async ping(signal?: AbortSignal): Promise<boolean> {
    const { error, response } = await this.client.GET("/ping", {
      parseAs: "text",
      signal,
    });
    throwOnOpenApiFetchError({ error, response }, "Execd ping failed");
    return true;
  }

  async *runStream(req: ApiRunCodeRequest, signal?: AbortSignal): AsyncIterable<ServerStreamEvent> {
    const url = joinUrl(this.opts.baseUrl, "/code");
    const body = JSON.stringify(req);
    const res = await this.fetch(url, {
      method: "POST",
      headers: {
        "accept": "text/event-stream",
        "content-type": "application/json",
        ...(this.opts.headers ?? {}),
      },
      body,
      signal,
    });

    for await (const ev of parseJsonEventStream<ServerStreamEvent>(res, { fallbackErrorMessage: "Run code failed" })) {
      yield ev;
    }
  }

  async run(
    code: string,
    opts: { context?: CodeContext; language?: SupportedLanguage; handlers?: ExecutionHandlers; signal?: AbortSignal } = {},
  ): Promise<Execution> {
    if (!code.trim()) {
      throw new InvalidArgumentException({ message: "Code cannot be empty" });
    }

    if (opts.context && opts.language) {
      throw new InvalidArgumentException({ message: "Provide either opts.context or opts.language, not both" });
    }

    const context: CodeContext =
      opts.context ??
      (opts.language
        ? { language: opts.language }
        : { language: "python" });

    // Make the OpenAPI contract explicit so backend schema changes surface quickly.
    const req: ApiRunCodeRequest = {
      code,
      context: { id: context.id, language: context.language },
    };

    const execution: Execution = {
      logs: { stdout: [], stderr: [] },
      result: [],
    };
    const dispatcher = new ExecutionEventDispatcher(execution, opts.handlers);

    for await (const ev of this.runStream(req, opts.signal)) {
      await dispatcher.dispatch(ev as any);
    }

    return execution;
  }
}