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

import type {
  CreateSnapshotRequest,
  CreateSandboxFromTemplateRequest,
  CreateSandboxRequest,
  CreateSandboxResponse,
  Endpoint,
  ListSnapshotsParams,
  ListSnapshotsResponse,
  ListSandboxesParams,
  ListSandboxesResponse,
  RenewSandboxExpirationRequest,
  RenewSandboxExpirationResponse,
  SnapshotInfo,
  SandboxId,
  SandboxInfo,
  SandboxMetadataPatch,
} from "../models/sandboxes.js";
import type {
  CreateTemplateRequest,
  ListTemplatesParams,
  ListTemplatesResponse,
  TemplateInfo,
} from "../models/templates.js";

export interface Sandboxes {
  createSandbox(
    req: CreateSandboxRequest,
    signal?: AbortSignal,
  ): Promise<CreateSandboxResponse>;
  /**
   * Create a sandbox from a `Succeeded` fsb template.
   *
   * Template mode fixes the workload shape on the server: only metadata,
   * network policy and extensions may accompany the template id, and the
   * timeout is required.
   */
  createSandboxFromTemplate(
    req: CreateSandboxFromTemplateRequest,
    signal?: AbortSignal,
  ): Promise<CreateSandboxResponse>;
  getSandbox(sandboxId: SandboxId): Promise<SandboxInfo>;
  listSandboxes(params?: ListSandboxesParams): Promise<ListSandboxesResponse>;
  patchSandboxMetadata(
    sandboxId: SandboxId,
    patch: SandboxMetadataPatch,
  ): Promise<SandboxInfo>;
  deleteSandbox(sandboxId: SandboxId, signal?: AbortSignal): Promise<void>;

  pauseSandbox(sandboxId: SandboxId): Promise<void>;
  resumeSandbox(sandboxId: SandboxId): Promise<void>;

  renewSandboxExpiration(
    sandboxId: SandboxId,
    req: RenewSandboxExpirationRequest,
  ): Promise<RenewSandboxExpirationResponse>;

  createSnapshot(
    sandboxId: SandboxId,
    req?: CreateSnapshotRequest,
  ): Promise<SnapshotInfo>;

  getSnapshot(snapshotId: string): Promise<SnapshotInfo>;
  listSnapshots(params?: ListSnapshotsParams): Promise<ListSnapshotsResponse>;
  deleteSnapshot(snapshotId: string): Promise<void>;

  /**
   * Create a fsb template (golden-image build).
   *
   * The build is asynchronous: the response starts at `status.phase: Pending`;
   * poll `getTemplate` until `Succeeded` (or `Failed`).
   */
  createTemplate(req: CreateTemplateRequest): Promise<TemplateInfo>;
  /**
   * Get one template with its latest build status.
   */
  getTemplate(templateId: string): Promise<TemplateInfo>;
  /**
   * List the current tenant's templates with optional metadata filtering
   * (AND logic) and pagination.
   */
  listTemplates(params?: ListTemplatesParams): Promise<ListTemplatesResponse>;
  /**
   * Delete a template. Sandboxes already created from the template are unaffected.
   */
  deleteTemplate(templateId: string): Promise<void>;

  getSandboxEndpoint(
    sandboxId: SandboxId,
    port: number,
    useServerProxy?: boolean,
    signal?: AbortSignal,
  ): Promise<Endpoint>;

  getSignedEndpoint(
    sandboxId: SandboxId,
    port: number,
    expires: number
  ): Promise<Endpoint>;

  invalidateEndpointCache?(sandboxId: SandboxId): void;
}
