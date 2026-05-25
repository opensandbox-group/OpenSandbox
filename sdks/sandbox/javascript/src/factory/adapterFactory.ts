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

import type { ConnectionConfig } from "../config/connection.js";
import type { SandboxFiles } from "../services/filesystem.js";
import type { Egress } from "../services/egress.js";
import type { ExecdCommands } from "../services/execdCommands.js";
import type { ExecdHealth } from "../services/execdHealth.js";
import type { ExecdMetrics } from "../services/execdMetrics.js";
import type { Sandboxes } from "../services/sandboxes.js";

export interface CreateLifecycleStackOptions {
  connectionConfig: ConnectionConfig;
  lifecycleBaseUrl: string;
}

export interface LifecycleStack {
  sandboxes: Sandboxes;
}

export interface CreateExecdStackOptions {
  connectionConfig: ConnectionConfig;
  execdBaseUrl: string;
  endpointHeaders?: Record<string, string>;
}

export interface ExecdStack {
  commands: ExecdCommands;
  files: SandboxFiles;
  health: ExecdHealth;
  metrics: ExecdMetrics;
}

export interface CreateEgressStackOptions {
  connectionConfig: ConnectionConfig;
  egressBaseUrl: string;
  endpointHeaders?: Record<string, string>;
}

export interface EgressStack {
  egress: Egress;
}

/**
 * Factory abstraction to keep `Sandbox` and `SandboxManager` decoupled from concrete adapter implementations.
 *
 * This is primarily useful for advanced integrations (custom transports, dependency injection, testing).
 */
export interface AdapterFactory {
  createLifecycleStack(opts: CreateLifecycleStackOptions): LifecycleStack;
  createExecdStack(opts: CreateExecdStackOptions): ExecdStack;
  createEgressStack(opts: CreateEgressStackOptions): EgressStack;
}
