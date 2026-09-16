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
	"fmt"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
	"github.com/alibaba/OpenSandbox/sandbox-k8s/internal/utils"
)

// applyPoolRefGuard reports an unsupported pool change while keeping the live
// allocation serving. The requested spec remains visible for the user to correct.
func (r *BatchSandboxReconciler) applyPoolRefGuard(sandbox *sandboxv1alpha1.BatchSandbox, status *sandboxv1alpha1.BatchSandboxStatus) {
	effective := utils.EffectivePoolRef(sandbox)
	condition := sandboxv1alpha1.BatchSandboxConditionPoolRefUpdateRejected
	if effective == sandbox.Spec.PoolRef {
		setConditionInStatus(status, condition, sandboxv1alpha1.ConditionFalse, "", "")
		return
	}
	message := fmt.Sprintf("spec.poolRef %q cannot replace allocated Pool %q; restore spec.poolRef to the allocated Pool", sandbox.Spec.PoolRef, effective)
	if r.Recorder != nil {
		reported := false
		for _, c := range sandbox.Status.Conditions {
			if c.Type == condition && c.Status == sandboxv1alpha1.ConditionTrue && c.Message == message {
				reported = true
				break
			}
		}
		if !reported {
			r.Recorder.Event(sandbox, corev1.EventTypeWarning, string(condition), message)
		}
	}
	setConditionInStatus(status, condition, sandboxv1alpha1.ConditionTrue, string(condition), message)
}
