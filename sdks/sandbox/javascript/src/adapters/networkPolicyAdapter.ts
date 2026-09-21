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
import type { paths as LifecyclePaths } from "../api/lifecycle.js";
import type { SandboxId, NetworkPolicy, NetworkRule } from "../models/sandboxes.js";
import type { Egress } from "../services/egress.js";

type ApiGetNetworkPolicyOk =
  LifecyclePaths["/sandboxes/{sandboxId}/networkpolicy"]["get"]["responses"][200]["content"]["application/json"];
type ApiPatchNetworkPolicyRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/networkpolicy"]["patch"]["requestBody"]["content"]["application/json"];
type ApiDeleteNetworkPolicyRequest =
  LifecyclePaths["/sandboxes/{sandboxId}/networkpolicy"]["delete"]["requestBody"]["content"]["application/json"];

/**
 * Egress policy operations routed through the lifecycle control plane.
 *
 * Implements the Egress protocol against the lifecycle server
 * (`/sandboxes/{sandboxId}/networkpolicy`) instead of the sandbox-side
 * egress sidecar. Used for sandboxes created from fsb templates, where the
 * policy intent is persisted server-side.
 *
 * The lifecycle server exposes the same merge/delete rule semantics as the
 * sidecar policy API (PATCH merges with first-wins-per-target; DELETE
 * removes by target idempotently), so no client-side read-modify-write is
 * needed.
 */
export class NetworkPolicyAdapter implements Egress {
  constructor(
    private readonly client: LifecycleClient,
    private readonly sandboxId: SandboxId,
  ) {}

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
