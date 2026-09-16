// Copyright 2025 Alibaba Group Holding Ltd.
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

package utils

import (
	"encoding/json"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

// AnnotationAllocationStatus records the Pool controller's committed allocation.
const AnnotationAllocationStatus = "sandbox.opensandbox.io/alloc-status"

// EffectivePoolRef keeps an allocated sandbox attached to its capacity source.
// An empty spec explicitly detaches it. Legacy records without a poolRef retain
// the spec-based behavior until the Pool controller backfills their allocation.
func EffectivePoolRef(sandbox *sandboxv1alpha1.BatchSandbox) string {
	if sandbox.Spec.PoolRef == "" {
		return ""
	}
	var allocation struct {
		PoolRef string `json:"poolRef"`
	}
	if err := json.Unmarshal([]byte(sandbox.Annotations[AnnotationAllocationStatus]), &allocation); err == nil && allocation.PoolRef != "" && allocation.PoolRef != "*" {
		return allocation.PoolRef
	}
	return sandbox.Spec.PoolRef
}
