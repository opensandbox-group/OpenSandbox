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

import { createExecdClient } from "../openapi/execdClient.js";
import { createEgressClient } from "../openapi/egressClient.js";
import { createLifecycleClient } from "../openapi/lifecycleClient.js";

import { CommandsAdapter } from "../adapters/commandsAdapter.js";
import { EgressAdapter } from "../adapters/egressAdapter.js";
import { FilesystemAdapter } from "../adapters/filesystemAdapter.js";
import { HealthAdapter } from "../adapters/healthAdapter.js";
import { IsolatedSessionsAdapter } from "../adapters/isolatedSessionsAdapter.js";
import { MetricsAdapter } from "../adapters/metricsAdapter.js";
import { SandboxesAdapter } from "../adapters/sandboxesAdapter.js";

import type {
  AdapterFactory,
  CreateEgressStackOptions,
  CreateExecdStackOptions,
  CreateLifecycleStackOptions,
  EgressStack,
  ExecdStack,
  LifecycleStack,
} from "./adapterFactory.js";

const API_KEY_HEADER = "OPEN-SANDBOX-API-KEY";

function createDataPlaneHeaders(
  connectionHeaders: Record<string, string>,
  endpointHeaders: Record<string, string> | undefined,
  useServerProxy: boolean,
  apiKey: string | undefined,
): Record<string, string> {
  const headers: Record<string, string> = {
    ...connectionHeaders,
    ...(endpointHeaders ?? {}),
  };
  const endpointApiKey = Object.entries(endpointHeaders ?? {}).find(
    ([key]) => key.toLowerCase() === API_KEY_HEADER.toLowerCase(),
  );

  if (!useServerProxy || endpointApiKey || apiKey) {
    for (const key of Object.keys(headers)) {
      if (key.toLowerCase() === API_KEY_HEADER.toLowerCase()) {
        delete headers[key];
      }
    }
  }

  if (useServerProxy) {
    if (endpointApiKey) {
      headers[endpointApiKey[0]] = endpointApiKey[1];
    } else if (apiKey) {
      headers[API_KEY_HEADER] = apiKey;
    }
  }

  return headers;
}

export class DefaultAdapterFactory implements AdapterFactory {
  createLifecycleStack(opts: CreateLifecycleStackOptions): LifecycleStack {
    const lifecycleClient = createLifecycleClient({
      baseUrl: opts.lifecycleBaseUrl,
      apiKey: opts.connectionConfig.apiKey,
      headers: opts.connectionConfig.headers,
      fetch: opts.connectionConfig.fetch,
    });
    const sandboxes = new SandboxesAdapter(lifecycleClient, {
      ttlMs: opts.connectionConfig.endpointCacheTtlMs,
      maxSize: opts.connectionConfig.endpointCacheSize,
      disabled: opts.connectionConfig.endpointCacheDisabled,
    });
    return { sandboxes };
  }

  createExecdStack(opts: CreateExecdStackOptions): ExecdStack {
    const headers = createDataPlaneHeaders(
      opts.connectionConfig.headers,
      opts.endpointHeaders,
      opts.connectionConfig.useServerProxy,
      opts.connectionConfig.apiKey,
    );
    const execdClient = createExecdClient({
      baseUrl: opts.execdBaseUrl,
      headers,
      fetch: opts.connectionConfig.fetch,
    });

    const health = new HealthAdapter(execdClient);
    const metrics = new MetricsAdapter(execdClient);
    const files = new FilesystemAdapter(execdClient, {
      baseUrl: opts.execdBaseUrl,
      fetch: opts.connectionConfig.fetch,
      headers,
    });
    const commands = new CommandsAdapter(execdClient, {
      baseUrl: opts.execdBaseUrl,
      fetch: opts.connectionConfig.sseFetch,
      headers,
    });

    const isolated = new IsolatedSessionsAdapter({
      baseUrl: opts.execdBaseUrl,
      fetch: opts.connectionConfig.fetch,
      sseFetch: opts.connectionConfig.sseFetch,
      headers,
    });

    return {
      commands,
      files,
      health,
      metrics,
      isolation: isolated,
    };
  }

  createEgressStack(opts: CreateEgressStackOptions): EgressStack {
    const headers = createDataPlaneHeaders(
      opts.connectionConfig.headers,
      opts.endpointHeaders,
      opts.connectionConfig.useServerProxy,
      opts.connectionConfig.apiKey,
    );
    const egressClient = createEgressClient({
      baseUrl: opts.egressBaseUrl,
      headers,
      fetch: opts.connectionConfig.fetch,
    });
    const egress = new EgressAdapter(egressClient, {
      baseUrl: opts.egressBaseUrl,
      fetch: opts.connectionConfig.fetch,
      headers,
    });
    return {
      egress,
      credentialVault: egress,
    };
  }
}

export function createDefaultAdapterFactory(): AdapterFactory {
  return new DefaultAdapterFactory();
}
