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

	pkgutils "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/utils"
)

const (
	annoAllocStatusKey           = "sandbox.opensandbox.io/alloc-status"
	annoAllocReleaseKey          = "sandbox.opensandbox.io/alloc-release"
	annoAllocReleasedKey         = "sandbox.opensandbox.io/alloc-released"
	labelBatchSandboxPodIndexKey = "batch-sandbox.sandbox.opensandbox.io/pod-index"
	labelBatchSandboxNameKey     = "batch-sandbox.sandbox.opensandbox.io/name"
	labelPrivilegedNodeAccess    = "sandbox.opensandbox.io/privileged-node-access"

	finalizerTaskCleanup    = "batch-sandbox.sandbox.opensandbox.io/task-cleanup"
	finalizerPoolAllocation = "pool.sandbox.opensandbox.io/pool-allocation"
)

// annotationSandboxEndpoints Use the exported constant from pkg/utils
var annotationSandboxEndpoints = pkgutils.AnnotationEndpoints

type sandboxAllocation struct {
	Pods       []string `json:"pods"`
	PoolRef    string   `json:"poolRef"`
	Generation int64    `json:"generation"`
}

type allocationRelease struct {
	Pods []string `json:"pods"`
}

type allocationReleased struct {
	Pods []string `json:"pods"`
}

type poolAllocation struct {
	PodAllocation map[string]string `json:"podAllocation"`
}

func parseSandboxAllocation(obj metav1.Object) (sandboxAllocation, error) {
	ret := sandboxAllocation{}
	if raw := obj.GetAnnotations()[annoAllocStatusKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &ret); err != nil {
			return ret, err
		}
	}
	return ret, nil
}

func parseSandboxReleased(obj metav1.Object) (allocationRelease, error) {
	ret := allocationRelease{}
	if raw := obj.GetAnnotations()[annoAllocReleaseKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &ret); err != nil {
			return ret, err
		}
	}
	return ret, nil
}
