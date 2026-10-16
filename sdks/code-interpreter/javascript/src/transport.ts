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

import type { Sandbox } from "@alibaba-group/opensandbox";
import { createRedirectSafeFetch } from "@alibaba-group/opensandbox/internal";

/**
 * Redirect-safe execd transport for the code-interpreter endpoint.
 *
 * Shared by the request path (`DefaultAdapterFactory.createCodes`) and the
 * health-check path (`CodeInterpreter`) so both handle credentials identically
 * and cannot drift apart.
 */
export function createRedirectSafeExecdFetch(
  sandbox: Sandbox,
  baseUrl: string,
): typeof fetch {
  return createRedirectSafeFetch(baseUrl, sandbox.connectionConfig.fetch, {
    overallTimeoutMs: sandbox.connectionConfig.requestTimeoutSeconds * 1000,
  });
}

/** Streaming (SSE) variant: no overall chain deadline for long-lived streams. */
export function createRedirectSafeExecdSseFetch(
  sandbox: Sandbox,
  baseUrl: string,
): typeof fetch {
  return createRedirectSafeFetch(baseUrl, sandbox.connectionConfig.sseFetch);
}
