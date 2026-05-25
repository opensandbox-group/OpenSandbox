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

export {
  InvalidArgumentException,
  SandboxApiException,
  SandboxError,
  SandboxException,
  SandboxInternalException,
  SandboxReadyTimeoutException,
  SandboxUnhealthyException,
} from "./core/exceptions.js";

// Factory pattern (stable public interface; does NOT expose OpenAPI generated models).
export type { AdapterFactory } from "./factory/adapterFactory.js";
export { DefaultAdapterFactory, createDefaultAdapterFactory } from "./factory/defaultAdapterFactory.js";

export { ConnectionConfig } from "./config/connection.js";
export type { ConnectionConfigOptions, ConnectionProtocol } from "./config/connection.js";

export type {
  CreateSnapshotRequest,
  CreateSandboxRequest,
  CreateSandboxResponse,
  Endpoint,
  Host,
  ListSnapshotsParams,
  ListSnapshotsResponse,
  ListSandboxesParams,
  ListSandboxesResponse,
  NetworkPolicy,
  NetworkRule,
  NetworkRuleAction,
  OSSFS,
  PlatformSpec,
  PVC,
  RenewSandboxExpirationRequest,
  RenewSandboxExpirationResponse,
  SnapshotInfo,
  SnapshotState,
  SnapshotStatus,
  SandboxId,
  SandboxInfo,
  SandboxMetadataPatch,
  Volume,
} from "./models/sandboxes.js";

export type { Sandboxes } from "./services/sandboxes.js";

export { SandboxManager } from "./manager.js";
export type { SandboxFilter, SandboxManagerOptions } from "./manager.js";

export type { ExecdHealth } from "./services/execdHealth.js";
export type { ExecdMetrics } from "./services/execdMetrics.js";
export type {
  FileInfo,
  FileMetadata,
  Permission,
  RenameFileItem,
  ReplaceFileContentItem,
  SearchFilesResponse,
  FilesInfoResponse,
} from "./models/filesystem.js";

export type {
  CommandExecution,
  CommandLogs,
  CommandStatus,
  RunCommandOpts,
  ServerStreamEvent,
  CodeContextRequest,
  SupportedLanguage,
  Metrics,
  SandboxMetrics,
  PingResponse,
} from "./models/execd.js";
export type { ExecdCommands } from "./services/execdCommands.js";

export type {
  Execution,
  ExecutionComplete,
  ExecutionError,
  ExecutionHandlers,
  ExecutionInit,
  ExecutionResult,
  OutputMessage,
} from "./models/execution.js";
export { ExecutionEventDispatcher } from "./models/executionEventDispatcher.js";

export {
  DEFAULT_ENTRYPOINT,
  DEFAULT_EGRESS_PORT,
  DEFAULT_EXECD_PORT,
  DEFAULT_RESOURCE_LIMITS,
  DEFAULT_TIMEOUT_SECONDS,
  DEFAULT_READY_TIMEOUT_SECONDS,
  DEFAULT_HEALTH_CHECK_POLLING_INTERVAL_MILLIS,
  DEFAULT_REQUEST_TIMEOUT_SECONDS,
} from "./core/constants.js";

export type {
  SandboxConnectOptions,
  SandboxCreateOptions,
} from "./sandbox.js";
export { Sandbox } from "./sandbox.js";

export type {
  ContentReplaceEntry,
  MoveEntry,
  SearchEntry,
  SetPermissionEntry,
  WriteEntry,
} from "./models/filesystem.js";
export type { SandboxFiles } from "./services/filesystem.js";
