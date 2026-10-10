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

import { createExecdClient } from "../openapi/execdClient.js";
import { createEgressClient } from "../openapi/egressClient.js";
import { createLifecycleClient } from "../openapi/lifecycleClient.js";
import { createRedirectSafeFetch } from "../config/redirectFetch.js";

import { CommandsAdapter } from "../adapters/commandsAdapter.js";
import { EgressAdapter } from "../adapters/egressAdapter.js";
import { FilesystemAdapter } from "../adapters/filesystemAdapter.js";
import { HealthAdapter } from "../adapters/healthAdapter.js";
import { IsolatedSessionsAdapter } from "../adapters/isolatedSessionsAdapter.js";
import { MetricsAdapter } from "../adapters/metricsAdapter.js";
import { NetworkPolicyAdapter } from "../adapters/networkPolicyAdapter.js";
import { SandboxesAdapter } from "../adapters/sandboxesAdapter.js";

import type {
  AdapterFactory,
  CreateEgressStackOptions,
  CreateExecdStackOptions,
  CreateLifecycleStackOptions,
  CreateNetworkPolicyStackOptions,
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
      fetch: createRedirectSafeFetch(
        opts.lifecycleBaseUrl,
        opts.connectionConfig.fetch,
      ),
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
    const execdFetch = createRedirectSafeFetch(
      opts.execdBaseUrl,
      opts.connectionConfig.fetch,
    );
    const execdSseFetch = createRedirectSafeFetch(
      opts.execdBaseUrl,
      opts.connectionConfig.sseFetch,
    );
    const execdClient = createExecdClient({
      baseUrl: opts.execdBaseUrl,
      headers,
      fetch: execdFetch,
    });

    const health = new HealthAdapter(execdClient);
    const metrics = new MetricsAdapter(execdClient);
    const files = new FilesystemAdapter(execdClient, {
      baseUrl: opts.execdBaseUrl,
      fetch: execdFetch,
      headers,
    });
    const commands = new CommandsAdapter(execdClient, {
      baseUrl: opts.execdBaseUrl,
      fetch: execdSseFetch,
      headers,
    });

    const isolated = new IsolatedSessionsAdapter({
      baseUrl: opts.execdBaseUrl,
      fetch: execdFetch,
      sseFetch: execdSseFetch,
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
    const egressFetch = createRedirectSafeFetch(
      opts.egressBaseUrl,
      opts.connectionConfig.fetch,
    );
    const egressClient = createEgressClient({
      baseUrl: opts.egressBaseUrl,
      headers,
      fetch: egressFetch,
    });
    const egress = new EgressAdapter(egressClient, {
      baseUrl: opts.egressBaseUrl,
      fetch: egressFetch,
      headers,
    });
    return {
      egress,
      credentialVault: egress,
    };
  }

  createNetworkPolicyStack(opts: CreateNetworkPolicyStackOptions): EgressStack {
    const lifecycleClient = createLifecycleClient({
      baseUrl: opts.lifecycleBaseUrl,
      apiKey: opts.connectionConfig.apiKey,
      headers: opts.connectionConfig.headers,
      fetch: createRedirectSafeFetch(
        opts.lifecycleBaseUrl,
        opts.connectionConfig.fetch,
      ),
    });
    return {
      egress: new NetworkPolicyAdapter(lifecycleClient, opts.sandboxId),
    };
  }
}

export function createDefaultAdapterFactory(): AdapterFactory {
  return new DefaultAdapterFactory();
}
