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

import { createExecdClient } from "@alibaba-group/opensandbox/internal";
import type { AdapterFactory, CreateCodesStackOptions } from "./adapterFactory.js";
import { CodesAdapter } from "../adapters/codesAdapter.js";
import type { Codes } from "../services/codes.js";
import {
  createRedirectSafeExecdFetch,
  createRedirectSafeExecdSseFetch,
} from "../transport.js";

export class DefaultAdapterFactory implements AdapterFactory {
  createCodes(opts: CreateCodesStackOptions): Codes {
    const headers: Record<string, string> = {
      ...(opts.sandbox.connectionConfig.headers ?? {}),
      ...(opts.endpointHeaders ?? {}),
    };
    // Strip protected OpenSandbox headers before following a cross-origin
    // redirect, matching the sandbox SDK (issue #2170).
    const execdFetch = createRedirectSafeExecdFetch(opts.sandbox, opts.execdBaseUrl);
    const execdSseFetch = createRedirectSafeExecdSseFetch(opts.sandbox, opts.execdBaseUrl);
    const client = createExecdClient({
      baseUrl: opts.execdBaseUrl,
      headers,
      fetch: execdFetch,
    });

    return new CodesAdapter(client, {
      baseUrl: opts.execdBaseUrl,
      headers,
      // Streaming calls (SSE) use a dedicated fetch, aligned with Kotlin/Python SDKs.
      fetch: execdSseFetch,
    });
  }
}

export function createDefaultAdapterFactory(): AdapterFactory {
  return new DefaultAdapterFactory();
}
