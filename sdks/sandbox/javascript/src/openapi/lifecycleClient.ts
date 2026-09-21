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

import createClient from "openapi-fetch";
import type { Client } from "openapi-fetch";

import type { paths as LifecyclePaths } from "../api/lifecycle.js";

export type LifecycleClient = Client<LifecyclePaths>;

export interface CreateLifecycleClientOptions {
  /**
   * Base URL to OpenSandbox Lifecycle API, including the `/v1` prefix.
   * Example: `http://localhost:8080/v1`
   */
  baseUrl?: string;
  /**
   * API key for `OPEN-SANDBOX-API-KEY` header.
   * If omitted, reads from `process.env.OPEN_SANDBOX_API_KEY` when available.
   */
  apiKey?: string;
  /**
   * Extra headers applied to every request.
   */
  headers?: Record<string, string>;
  /**
   * Custom fetch implementation.
   *
   * Useful for proxies, custom TLS, request tracing, retries, or running in environments
   * where a global `fetch` is not available.
   */
  fetch?: typeof fetch;
}

function readEnvApiKey(): string | undefined {
  // Avoid requiring @types/node by not referencing `process` directly.
  // In Node, `globalThis.process.env` exists; in browsers it won't.
  const env = (globalThis as any)?.process?.env;
  const v = env?.OPEN_SANDBOX_API_KEY;
  return typeof v === "string" && v.length ? v : undefined;
}

export function createLifecycleClient(opts: CreateLifecycleClientOptions = {}): LifecycleClient {
  const apiKey = opts.apiKey ?? readEnvApiKey();

  const headers: Record<string, string> = {
    ...(opts.headers ?? {}),
  };

  if (apiKey && !headers["OPEN-SANDBOX-API-KEY"]) {
    headers["OPEN-SANDBOX-API-KEY"] = apiKey;
  }

  const createClientFn =
      (createClient as unknown as { default?: typeof createClient }).default ?? createClient;
  return createClientFn<LifecyclePaths>({
    baseUrl: opts.baseUrl ?? "http://localhost:8080/v1",
    headers,
    fetch: opts.fetch,
  });
}