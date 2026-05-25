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

import type { EgressClient } from "../openapi/egressClient.js";
import { throwOnOpenApiFetchError } from "./openapiError.js";
import type { paths as EgressPaths } from "../api/egress.js";
import type { NetworkPolicy, NetworkRule } from "../models/sandboxes.js";
import type { Egress } from "../services/egress.js";

type ApiGetPolicyOk =
  EgressPaths["/policy"]["get"]["responses"][200]["content"]["application/json"];
type ApiPatchRulesRequest =
  EgressPaths["/policy"]["patch"]["requestBody"]["content"]["application/json"];

export class EgressAdapter implements Egress {
  constructor(private readonly client: EgressClient) {}

  async getPolicy(): Promise<NetworkPolicy> {
    const { data, error, response } = await this.client.GET("/policy");
    throwOnOpenApiFetchError({ error, response }, "Get sandbox egress policy failed");
    const raw = data as ApiGetPolicyOk | undefined;
    if (!raw || typeof raw !== "object" || !raw.policy || typeof raw.policy !== "object") {
      throw new Error("Get sandbox egress policy failed: unexpected response shape");
    }
    return raw.policy as NetworkPolicy;
  }

  async patchRules(rules: NetworkRule[]): Promise<void> {
    const body: ApiPatchRulesRequest = rules as unknown as ApiPatchRulesRequest;
    const { error, response } = await this.client.PATCH("/policy", {
      body,
    });
    throwOnOpenApiFetchError({ error, response }, "Patch sandbox egress rules failed");
  }
}
