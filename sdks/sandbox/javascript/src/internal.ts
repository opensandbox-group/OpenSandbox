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

/**
 * INTERNAL / ADVANCED ENTRYPOINT
 *
 * This subpath exposes low-level OpenAPI clients and adapters for advanced integrations.
 * It is intentionally NOT exported from the root entrypoint (`@alibaba-group/opensandbox`),
 * because generated OpenAPI types are not considered stable public API.
 *
 * Import path:
 * - `@alibaba-group/opensandbox/internal`
 */

export { createLifecycleClient } from "./openapi/lifecycleClient.js";
export type { LifecycleClient } from "./openapi/lifecycleClient.js";
export { createExecdClient } from "./openapi/execdClient.js";
export type { ExecdClient } from "./openapi/execdClient.js";
export { createEgressClient } from "./openapi/egressClient.js";
export type { EgressClient } from "./openapi/egressClient.js";

// OpenAPI schema types (NOT stable public API; internal-only).
export type { paths as LifecyclePaths } from "./api/lifecycle.js";
export type { paths as ExecdPaths } from "./api/execd.js";
export type { paths as EgressPaths } from "./api/egress.js";

export { SandboxesAdapter } from "./adapters/sandboxesAdapter.js";
export { EgressAdapter } from "./adapters/egressAdapter.js";
export type { EgressRawHttpOptions } from "./adapters/egressAdapter.js";
export { HealthAdapter } from "./adapters/healthAdapter.js";
export { MetricsAdapter } from "./adapters/metricsAdapter.js";
export { FilesystemAdapter } from "./adapters/filesystemAdapter.js";
export { CommandsAdapter } from "./adapters/commandsAdapter.js";
export { PtyAdapter, UnavailablePtyAdapter } from "./adapters/ptyAdapter.js";
