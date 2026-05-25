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

import type { LifecycleClient } from "../openapi/lifecycleClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { paths as LifecyclePaths } from "../api/lifecycle.js";
import type {
  Sandboxes,
} from "../services/sandboxes.js";
import type {
  CreateSnapshotRequest,
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

type ApiCreateSandboxRequest =
  LifecyclePaths["/sandboxes"]["post"]["requestBody"]["content"]["application/json"];
type ApiCreateSandboxOk =
  LifecyclePaths["/sandboxes"]["post"]["responses"][202]["content"]["application/json"];
type ApiGetSandboxOk =
  LifecyclePaths["/sandboxes/{sandboxId}"]["get"]["responses"][200]["content"]["application/json"];
type ApiListSandboxesOk =
  LifecyclePaths["/sandboxes"]["get"]["responses"][200]["content"]["application/json"];
type ApiPatchSandboxMetadataRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/metadata"]["patch"]["requestBody"]["content"]["application/json"];
type ApiPatchSandboxMetadataOk =
  LifecyclePaths["/sandboxes/{sandboxId}/metadata"]["patch"]["responses"][200]["content"]["application/json"];
type ApiRenewSandboxExpirationRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/renew-expiration"]["post"]["requestBody"]["content"]["application/json"];
type ApiRenewSandboxExpirationOk =
  LifecyclePaths["/sandboxes/{sandboxId}/renew-expiration"]["post"]["responses"][200]["content"]["application/json"];
type ApiCreateSnapshotRequest =
  NonNullable<
    LifecyclePaths["/sandboxes/{sandboxId}/snapshots"]["post"]["requestBody"]
  >["content"]["application/json"];
type ApiCreateSnapshotOk =
  LifecyclePaths["/sandboxes/{sandboxId}/snapshots"]["post"]["responses"][202]["content"]["application/json"];
type ApiGetSnapshotOk =
  LifecyclePaths["/snapshots/{snapshotId}"]["get"]["responses"][200]["content"]["application/json"];
type ApiListSnapshotsOk =
  LifecyclePaths["/snapshots"]["get"]["responses"][200]["content"]["application/json"];
type ApiEndpointOk =
  LifecyclePaths["/sandboxes/{sandboxId}/endpoints/{port}"]["get"]["responses"][200]["content"]["application/json"];

function encodeMetadataFilter(metadata: Record<string, string>): string {
  // The Lifecycle API expects a single `metadata` query parameter whose value is `k=v&k2=v2`.
  // The query serializer will URL-encode the value (e.g. `=` -> %3D and `&` -> %26).
  const parts: string[] = [];
  for (const [k, v] of Object.entries(metadata)) {
    parts.push(`${k}=${v}`);
  }
  return parts.join("&");
}

export class SandboxesAdapter implements Sandboxes {
  constructor(private readonly client: LifecycleClient) {}

  private parseIsoDate(field: string, v: unknown): Date {
    if (typeof v !== "string" || !v) {
      throw new Error(`Invalid ${field}: expected ISO string, got ${typeof v}`);
    }
    const d = new Date(v);
    if (Number.isNaN(d.getTime())) {
      throw new Error(`Invalid ${field}: ${v}`);
    }
    return d;
  }

  private parseOptionalIsoDate(field: string, v: unknown): Date | null {
    if (v == null) return null;
    return this.parseIsoDate(field, v);
  }

  private mapSnapshotInfo(raw: ApiGetSnapshotOk | ApiCreateSnapshotOk): SnapshotInfo {
    return {
      ...(raw ?? {}),
      createdAt: this.parseIsoDate("createdAt", raw?.createdAt),
      status: {
        ...(raw?.status ?? {}),
        lastTransitionAt: raw?.status?.lastTransitionAt == null
          ? undefined
          : this.parseIsoDate("lastTransitionAt", raw.status.lastTransitionAt),
      },
    } as SnapshotInfo;
  }

  private mapSandboxInfo(raw: ApiGetSandboxOk): SandboxInfo {
    return {
      ...(raw ?? {}),
      createdAt: this.parseIsoDate("createdAt", raw?.createdAt),
      expiresAt: this.parseOptionalIsoDate("expiresAt", raw?.expiresAt),
    } as SandboxInfo;
  }

  async createSandbox(req: CreateSandboxRequest): Promise<CreateSandboxResponse> {
    // Make the OpenAPI contract explicit so backend schema changes surface quickly.
    const body: ApiCreateSandboxRequest = req as unknown as ApiCreateSandboxRequest;
    const { data, error, response } = await this.client.POST("/sandboxes", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Create sandbox failed");
    const raw = data as ApiCreateSandboxOk | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("Create sandbox failed: unexpected response shape");
    }
    return {
      ...(raw ?? {}),
      createdAt: this.parseIsoDate("createdAt", raw?.createdAt),
      expiresAt: this.parseOptionalIsoDate("expiresAt", raw?.expiresAt),
    } as CreateSandboxResponse;
  }

  async getSandbox(sandboxId: SandboxId): Promise<SandboxInfo> {
    const { data, error, response } = await this.client.GET("/sandboxes/{sandboxId}", {
      params: { path: { sandboxId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get sandbox failed");
    const ok = data as ApiGetSandboxOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Get sandbox failed: unexpected response shape");
    }
    return this.mapSandboxInfo(ok);
  }

  async listSandboxes(params: ListSandboxesParams = {}): Promise<ListSandboxesResponse> {
    const query: Record<string, string | number | boolean | undefined | null | (string | number)[]> = {};
    if (params.states?.length) query.state = params.states;
    if (params.metadata && Object.keys(params.metadata).length) {
      query.metadata = encodeMetadataFilter(params.metadata);
    }
    if (params.page != null) query.page = params.page;
    if (params.pageSize != null) query.pageSize = params.pageSize;

    const { data, error, response } = await this.client.GET("/sandboxes", {
      params: { query },
    });
    throwOnOpenApiFetchError({ error, response }, "List sandboxes failed");
    const raw = data as ApiListSandboxesOk | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("List sandboxes failed: unexpected response shape");
    }
    const itemsRaw = raw.items;
    if (!Array.isArray(itemsRaw)) throw new Error("List sandboxes failed: unexpected items shape");
    return {
      ...(raw ?? {}),
      items: itemsRaw.map((x) => this.mapSandboxInfo(x)),
    } as ListSandboxesResponse;
  }

  async patchSandboxMetadata(
    sandboxId: SandboxId,
    patch: SandboxMetadataPatch,
  ): Promise<SandboxInfo> {
    const body: ApiPatchSandboxMetadataRequest = patch;
    const { data, error, response } = await this.client.PATCH("/sandboxes/{sandboxId}/metadata", {
      params: { path: { sandboxId } },
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Patch sandbox metadata failed");
    const ok = data as ApiPatchSandboxMetadataOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Patch sandbox metadata failed: unexpected response shape");
    }
    return this.mapSandboxInfo(ok);
  }

  async deleteSandbox(sandboxId: SandboxId): Promise<void> {
    const { error, response } = await this.client.DELETE("/sandboxes/{sandboxId}", {
      params: { path: { sandboxId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Delete sandbox failed");
  }

  async pauseSandbox(sandboxId: SandboxId): Promise<void> {
    const { error, response } = await this.client.POST("/sandboxes/{sandboxId}/pause", {
      params: { path: { sandboxId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Pause sandbox failed");
  }

  async resumeSandbox(sandboxId: SandboxId): Promise<void> {
    const { error, response } = await this.client.POST("/sandboxes/{sandboxId}/resume", {
      params: { path: { sandboxId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Resume sandbox failed");
  }

  async renewSandboxExpiration(
    sandboxId: SandboxId,
    req: RenewSandboxExpirationRequest,
  ): Promise<RenewSandboxExpirationResponse> {
    const body: ApiRenewSandboxExpirationRequest = req as unknown as ApiRenewSandboxExpirationRequest;
    const { data, error, response } = await this.client.POST("/sandboxes/{sandboxId}/renew-expiration", {
      params: { path: { sandboxId } },
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Renew sandbox expiration failed");
    const raw = data as ApiRenewSandboxExpirationOk | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("Renew sandbox expiration failed: unexpected response shape");
    }
    return {
      ...(raw ?? {}),
      expiresAt: raw?.expiresAt ? this.parseIsoDate("expiresAt", raw.expiresAt) : undefined,
    } as RenewSandboxExpirationResponse;
  }

  async createSnapshot(
    sandboxId: SandboxId,
    req: CreateSnapshotRequest = {},
  ): Promise<SnapshotInfo> {
    const body: ApiCreateSnapshotRequest = req as unknown as ApiCreateSnapshotRequest;
    const { data, error, response } = await this.client.POST("/sandboxes/{sandboxId}/snapshots", {
      params: { path: { sandboxId } },
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Create snapshot failed");
    const raw = data as ApiCreateSnapshotOk | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("Create snapshot failed: unexpected response shape");
    }
    return this.mapSnapshotInfo(raw);
  }

  async getSnapshot(snapshotId: string): Promise<SnapshotInfo> {
    const { data, error, response } = await this.client.GET("/snapshots/{snapshotId}", {
      params: { path: { snapshotId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get snapshot failed");
    const raw = data as ApiGetSnapshotOk | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("Get snapshot failed: unexpected response shape");
    }
    return this.mapSnapshotInfo(raw);
  }

  async listSnapshots(params: ListSnapshotsParams = {}): Promise<ListSnapshotsResponse> {
    const query: Record<string, string | number | (string | number)[] | undefined> = {};
    if (params.sandboxId) query.sandboxId = params.sandboxId;
    if (params.states?.length) query.state = params.states;
    if (params.page != null) query.page = params.page;
    if (params.pageSize != null) query.pageSize = params.pageSize;

    const { data, error, response } = await this.client.GET("/snapshots", {
      params: { query },
    });
    throwOnOpenApiFetchError({ error, response }, "List snapshots failed");
    const raw = data as ApiListSnapshotsOk | undefined;
    if (!raw || typeof raw !== "object") {
      throw new Error("List snapshots failed: unexpected response shape");
    }
    const itemsRaw = raw.items;
    if (!Array.isArray(itemsRaw)) throw new Error("List snapshots failed: unexpected items shape");
    return {
      ...(raw ?? {}),
      items: itemsRaw.map((x) => this.mapSnapshotInfo(x)),
    } as ListSnapshotsResponse;
  }

  async deleteSnapshot(snapshotId: string): Promise<void> {
    const { error, response } = await this.client.DELETE("/snapshots/{snapshotId}", {
      params: { path: { snapshotId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Delete snapshot failed");
  }

  async getSandboxEndpoint(
    sandboxId: SandboxId,
    port: number,
    useServerProxy = false
  ): Promise<Endpoint> {
    const { data, error, response } = await this.client.GET("/sandboxes/{sandboxId}/endpoints/{port}", {
      params: { path: { sandboxId, port }, query: { use_server_proxy: useServerProxy } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get sandbox endpoint failed");
    const ok = data as ApiEndpointOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Get sandbox endpoint failed: unexpected response shape");
    }
    return ok as unknown as Endpoint;
  }

  async getSignedEndpoint(
    sandboxId: SandboxId,
    port: number,
    expires: number
  ): Promise<Endpoint> {
    const { data, error, response } = await this.client.GET("/sandboxes/{sandboxId}/endpoints/{port}", {
      params: { path: { sandboxId, port }, query: { expires: expires.toString() } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get signed endpoint failed");
    const ok = data as ApiEndpointOk | undefined;
    if (!ok || typeof ok !== "object") {
      throw new Error("Get signed endpoint failed: unexpected response shape");
    }
    return ok as unknown as Endpoint;
  }
}
