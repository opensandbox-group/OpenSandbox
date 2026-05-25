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

package controller

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
	pkgutils "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/utils"
)

const (
	AnnoAllocStatusKey           = "sandbox.opensandbox.io/alloc-status"
	AnnoAllocReleaseKey          = "sandbox.opensandbox.io/alloc-release"
	AnnoAllocReleasedKey         = "sandbox.opensandbox.io/alloc-released"
	LabelBatchSandboxPodIndexKey = "batch-sandbox.sandbox.opensandbox.io/pod-index"
	LabelBatchSandboxNameKey     = "batch-sandbox.sandbox.opensandbox.io/name"
	LabelPrivilegedNodeAccess    = "sandbox.opensandbox.io/privileged-node-access"

	FinalizerTaskCleanup    = "batch-sandbox.sandbox.opensandbox.io/task-cleanup"
	FinalizerPoolAllocation = "pool.sandbox.opensandbox.io/pool-allocation"
)

// AnnotationSandboxEndpoints Use the exported constant from pkg/utils
var AnnotationSandboxEndpoints = pkgutils.AnnotationEndpoints

type SandboxAllocation struct {
	Pods []string `json:"pods"`
}

type AllocationRelease struct {
	Pods []string `json:"pods"`
}

type AllocationReleased struct {
	Pods []string `json:"pods"`
}

type PoolAllocation struct {
	PodAllocation map[string]string `json:"podAllocation"`
}

func parseSandboxAllocation(obj metav1.Object) (SandboxAllocation, error) {
	ret := SandboxAllocation{}
	if raw := obj.GetAnnotations()[AnnoAllocStatusKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &ret); err != nil {
			return ret, err
		}
	}
	return ret, nil
}

func setSandboxAllocation(obj metav1.Object, alloc SandboxAllocation) {
	if obj.GetAnnotations() == nil {
		obj.SetAnnotations(map[string]string{})
	}
	obj.GetAnnotations()[AnnoAllocStatusKey] = utils.DumpJSON(alloc)
}

func parseSandboxReleased(obj metav1.Object) (AllocationRelease, error) {
	ret := AllocationRelease{}
	if raw := obj.GetAnnotations()[AnnoAllocReleaseKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &ret); err != nil {
			return ret, err
		}
	}
	return ret, nil
}
