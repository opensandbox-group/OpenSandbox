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

import type { NetworkPolicy, NetworkRule } from "../models/sandboxes.js";

export interface Egress {
  getPolicy(): Promise<NetworkPolicy>;
  /**
   * Patch egress rules with sidecar merge semantics.
   *
   * Incoming rules take priority over existing rules with the same target.
   * Existing rules for other targets remain unchanged. Within one patch payload,
   * the first rule for a target wins. The current defaultAction is preserved.
   */
  patchRules(rules: NetworkRule[]): Promise<void>;
}
