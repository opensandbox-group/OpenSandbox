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

import type { LifecycleClient } from "../openapi/lifecycleClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import {
  sanitizeCredentialBindingListResponse,
  sanitizeCredentialBindingMetadata,
  sanitizeCredentialListResponse,
  sanitizeCredentialMetadata,
  sanitizeCredentialVaultState,
} from "./egressAdapter.js";
import type { paths as LifecyclePaths } from "../api/lifecycle.js";
import type {
  CredentialBindingMetadata,
  CredentialMetadata,
  CredentialVaultCreateRequest,
  CredentialVaultPatchRequest,
  CredentialVaultState,
  NetworkPolicy,
  NetworkRule,
  SandboxId,
} from "../models/sandboxes.js";
import type { CredentialVault, Egress } from "../services/egress.js";

type ApiGetNetworkPolicyOk =
  LifecyclePaths["/sandboxes/{sandboxId}/networkpolicy"]["get"]["responses"][200]["content"]["application/json"];
type ApiPatchNetworkPolicyRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/networkpolicy"]["patch"]["requestBody"]["content"]["application/json"];
type ApiDeleteNetworkPolicyRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/networkpolicy"]["delete"]["requestBody"]["content"]["application/json"];
type ApiCreateCredentialVaultRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/credential-vault"]["post"]["requestBody"]["content"]["application/json"];
type ApiPatchCredentialVaultRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/credential-vault"]["patch"]["requestBody"]["content"]["application/json"];

/**
 * Egress policy and Credential Vault operations routed through the lifecycle
 * control plane.
 *
 * Implements the Egress protocol against the lifecycle server
 * (`/sandboxes/{sandboxId}/networkpolicy`) and the server-routed Credential
 * Vault (`/sandboxes/{sandboxId}/credential-vault`, OSEP-0022) instead of the
 * sandbox-side egress sidecar. Used for sandboxes created from fsb templates,
 * where the policy intent is persisted server-side and the vault is
 * egress-held behind the server.
 *
 * The lifecycle server exposes the same merge/delete rule semantics as the
 * sidecar policy API (PATCH merges with first-wins-per-target; DELETE removes
 * by target idempotently), so no client-side read-modify-write is needed. The
 * server-side vault PATCH is equally atomic (composed server-side over the
 * FastPath relay); a 412 means another writer won the revision — re-read and
 * retry with the fresh `expectedRevision`.
 */
export class NetworkPolicyAdapter implements Egress, CredentialVault {
  constructor(
    private readonly client: LifecycleClient,
    private readonly sandboxId: SandboxId,
  ) {}

  async create(
    request: CredentialVaultCreateRequest,
  ): Promise<CredentialVaultState> {
    const { data, error, response } = await this.client.POST(
      "/sandboxes/{sandboxId}/credential-vault",
      {
        params: { path: { sandboxId: this.sandboxId } },
        body: request as unknown as ApiCreateCredentialVaultRequest,
      },
    );
    throwOnOpenApiFetchError({ error, response }, "Create credential vault failed");
    return sanitizeCredentialVaultState(data, "Create credential vault");
  }

  async get(): Promise<CredentialVaultState> {
    const { data, error, response } = await this.client.GET(
      "/sandboxes/{sandboxId}/credential-vault",
      { params: { path: { sandboxId: this.sandboxId } } },
    );
    throwOnOpenApiFetchError({ error, response }, "Get credential vault failed");
    return sanitizeCredentialVaultState(data, "Get credential vault");
  }

  async patch(
    request: CredentialVaultPatchRequest,
  ): Promise<CredentialVaultState> {
    const { data, error, response } = await this.client.PATCH(
      "/sandboxes/{sandboxId}/credential-vault",
      {
        params: { path: { sandboxId: this.sandboxId } },
        body: request as unknown as ApiPatchCredentialVaultRequest,
      },
    );
    throwOnOpenApiFetchError({ error, response }, "Patch credential vault failed");
    return sanitizeCredentialVaultState(data, "Patch credential vault");
  }

  async delete(): Promise<void> {
    const { error, response } = await this.client.DELETE(
      "/sandboxes/{sandboxId}/credential-vault",
      { params: { path: { sandboxId: this.sandboxId } } },
    );
    throwOnOpenApiFetchError({ error, response }, "Delete credential vault failed");
  }

  async listCredentials(): Promise<CredentialMetadata[]> {
    const { data, error, response } = await this.client.GET(
      "/sandboxes/{sandboxId}/credential-vault/credentials",
      { params: { path: { sandboxId: this.sandboxId } } },
    );
    throwOnOpenApiFetchError(
      { error, response },
      "List credential vault credentials failed",
    );
    return sanitizeCredentialListResponse(
      data,
      "List credential vault credentials",
    );
  }

  async getCredential(name: string): Promise<CredentialMetadata> {
    const { data, error, response } = await this.client.GET(
      "/sandboxes/{sandboxId}/credential-vault/credentials/{credentialName}",
      {
        params: {
          path: { sandboxId: this.sandboxId, credentialName: name },
        },
      },
    );
    throwOnOpenApiFetchError(
      { error, response },
      "Get credential vault credential failed",
    );
    return sanitizeCredentialMetadata(
      data,
      "Get credential vault credential",
    );
  }

  async listBindings(): Promise<CredentialBindingMetadata[]> {
    const { data, error, response } = await this.client.GET(
      "/sandboxes/{sandboxId}/credential-vault/bindings",
      { params: { path: { sandboxId: this.sandboxId } } },
    );
    throwOnOpenApiFetchError(
      { error, response },
      "List credential vault bindings failed",
    );
    return sanitizeCredentialBindingListResponse(
      data,
      "List credential vault bindings",
    );
  }

  async getBinding(name: string): Promise<CredentialBindingMetadata> {
    const { data, error, response } = await this.client.GET(
      "/sandboxes/{sandboxId}/credential-vault/bindings/{bindingName}",
      {
        params: { path: { sandboxId: this.sandboxId, bindingName: name } },
      },
    );
    throwOnOpenApiFetchError(
      { error, response },
      "Get credential vault binding failed",
    );
    return sanitizeCredentialBindingMetadata(
      data,
      "Get credential vault binding",
    );
  }

  async getPolicy(): Promise<NetworkPolicy> {
    const { data, error, response } = await this.client.GET("/sandboxes/{sandboxId}/networkpolicy", {
      params: { path: { sandboxId: this.sandboxId } },
    });
    throwOnOpenApiFetchError({ error, response }, "Get sandbox network policy failed");
    const raw = data as ApiGetNetworkPolicyOk | undefined;
    if (!raw || typeof raw !== "object" || !raw.policy || typeof raw.policy !== "object") {
      throw new Error("Get sandbox network policy failed: unexpected response shape");
    }
    return raw.policy as NetworkPolicy;
  }

  async patchRules(rules: NetworkRule[]): Promise<void> {
    const body = rules as unknown as ApiPatchNetworkPolicyRequest;
    const { error, response } = await this.client.PATCH("/sandboxes/{sandboxId}/networkpolicy", {
      params: { path: { sandboxId: this.sandboxId } },
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Patch sandbox network policy rules failed");
  }

  async deleteRules(targets: string[]): Promise<void> {
    const body = targets as unknown as ApiDeleteNetworkPolicyRequest;
    const { error, response } = await this.client.DELETE("/sandboxes/{sandboxId}/networkpolicy", {
      params: { path: { sandboxId: this.sandboxId } },
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Delete sandbox network policy rules failed");
  }
}
