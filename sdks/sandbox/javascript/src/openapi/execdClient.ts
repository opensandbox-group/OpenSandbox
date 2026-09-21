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

import type { paths as ExecdPaths } from "../api/execd.js";

export type ExecdClient = Client<ExecdPaths>;

export interface CreateExecdClientOptions {
  /**
   * Base URL to the Execd API (no `/v1` prefix).
   * Examples:
   * - `http://localhost:44772`
   * - `http://api.opensandbox.io/sandboxes/<id>/port/44772`
   */
  baseUrl: string;
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

export function createExecdClient(opts: CreateExecdClientOptions): ExecdClient {
  const createClientFn =
      (createClient as unknown as { default?: typeof createClient }).default ?? createClient;
  return createClientFn<ExecdPaths>({
    baseUrl: opts.baseUrl,
    headers: opts.headers,
    fetch: opts.fetch,
  });
}