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

import { ConnectionConfig, type ConnectionConfigOptions } from "./config/connection.js";
import { createDefaultAdapterFactory } from "./factory/defaultAdapterFactory.js";
import type { AdapterFactory } from "./factory/adapterFactory.js";

import type {
  CreateSnapshotRequest,
  ListSandboxesResponse,
  ListSnapshotsParams,
  ListSnapshotsResponse,
  SandboxId,
  SandboxInfo,
  SandboxMetadataPatch,
  SnapshotInfo,
} from "./models/sandboxes.js";
import type { Sandboxes } from "./services/sandboxes.js";

export interface SandboxManagerOptions {
  /**
   * Connection configuration for calling the OpenSandbox Lifecycle API.
   */
  connectionConfig?: ConnectionConfig | ConnectionConfigOptions;
  /**
   * Advanced override: inject a custom adapter factory (custom transports, dependency injection).
   */
  adapterFactory?: AdapterFactory;
}

export interface SandboxFilter {
  /**
   * Filter by sandbox lifecycle states.
   */
  states?: string[];
  /**
   * Filter by metadata key-value pairs.
   */
  metadata?: Record<string, string>;
  /**
   * Pagination page number (1-indexed).
   */
  page?: number;
  /**
   * Number of items per page.
   */
  pageSize?: number;
}

/**
 * Administrative interface for managing sandboxes (list/get/pause/resume/kill/renew).
 *
 * For interacting *inside* a sandbox, use {@link Sandbox}.
 */
export class SandboxManager {
  private readonly sandboxes: Sandboxes;
  private readonly connectionConfig: ConnectionConfig;

  private constructor(opts: { sandboxes: Sandboxes; connectionConfig: ConnectionConfig }) {
    this.sandboxes = opts.sandboxes;
    this.connectionConfig = opts.connectionConfig;
  }

  static create(opts: SandboxManagerOptions = {}): SandboxManager {
    const baseConnectionConfig = opts.connectionConfig instanceof ConnectionConfig
      ? opts.connectionConfig
      : new ConnectionConfig(opts.connectionConfig);
    const connectionConfig = baseConnectionConfig.withTransportIfMissing();
    const lifecycleBaseUrl = connectionConfig.getBaseUrl();
    const adapterFactory = opts.adapterFactory ?? createDefaultAdapterFactory();
    let sandboxes: Sandboxes;
    try {
      sandboxes = adapterFactory.createLifecycleStack({
        connectionConfig,
        lifecycleBaseUrl,
      }).sandboxes;
    } catch (err) {
      void connectionConfig.closeTransport().catch(() => undefined);
      throw err;
    }
    return new SandboxManager({ sandboxes, connectionConfig });
  }

  listSandboxInfos(filter: SandboxFilter = {}): Promise<ListSandboxesResponse> {
    return this.sandboxes.listSandboxes({
      states: filter.states,
      metadata: filter.metadata,
      page: filter.page,
      pageSize: filter.pageSize,
    });
  }

  getSandboxInfo(sandboxId: SandboxId): Promise<SandboxInfo> {
    return this.sandboxes.getSandbox(sandboxId);
  }

  patchSandboxMetadata(
    sandboxId: SandboxId,
    patch: SandboxMetadataPatch,
  ): Promise<SandboxInfo> {
    return this.sandboxes.patchSandboxMetadata(sandboxId, patch);
  }

  killSandbox(sandboxId: SandboxId): Promise<void> {
    return this.sandboxes.deleteSandbox(sandboxId);
  }

  pauseSandbox(sandboxId: SandboxId): Promise<void> {
    return this.sandboxes.pauseSandbox(sandboxId);
  }

  resumeSandbox(sandboxId: SandboxId): Promise<void> {
    return this.sandboxes.resumeSandbox(sandboxId);
  }

  /**
   * Renew expiration by setting expiresAt to now + timeoutSeconds.
   */
  async renewSandbox(sandboxId: SandboxId, timeoutSeconds: number): Promise<void> {
    const expiresAt = new Date(Date.now() + timeoutSeconds * 1000).toISOString();
    await this.sandboxes.renewSandboxExpiration(sandboxId, { expiresAt });
  }

  createSnapshot(sandboxId: SandboxId, req?: CreateSnapshotRequest): Promise<SnapshotInfo> {
    return this.sandboxes.createSnapshot(sandboxId, req);
  }

  getSnapshot(snapshotId: string): Promise<SnapshotInfo> {
    return this.sandboxes.getSnapshot(snapshotId);
  }

  listSnapshots(filter: ListSnapshotsParams = {}): Promise<ListSnapshotsResponse> {
    return this.sandboxes.listSnapshots(filter);
  }

  deleteSnapshot(snapshotId: string): Promise<void> {
    return this.sandboxes.deleteSnapshot(snapshotId);
  }

  /**
   * Release the HTTP agent resources allocated for this manager instance.
   *
   * Each manager clone owns a scoped `ConnectionConfig` clone.
   *
   * This mirrors the Python SDK's default transport lifecycle.
   */
  async close(): Promise<void> {
    await this.connectionConfig.closeTransport();
  }
}
