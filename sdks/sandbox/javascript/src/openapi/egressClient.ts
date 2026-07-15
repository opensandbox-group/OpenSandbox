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

import createClient from "openapi-fetch";
import type { Client } from "openapi-fetch";

import type { paths as EgressPaths } from "../api/egress.js";

export type EgressClient = Client<EgressPaths>;

export interface CreateEgressClientOptions {
  /**
   * Base URL to the sandbox egress sidecar API.
   */
  baseUrl: string;
  /**
   * Extra headers applied to every request.
   */
  headers?: Record<string, string>;
  /**
   * Custom fetch implementation.
   */
  fetch?: typeof fetch;
}

export function createEgressClient(opts: CreateEgressClientOptions): EgressClient {
  const createClientFn =
    (createClient as unknown as { default?: typeof createClient }).default ?? createClient;
  return createClientFn<EgressPaths>({
    baseUrl: opts.baseUrl,
    headers: opts.headers,
    fetch: opts.fetch,
  });
}
