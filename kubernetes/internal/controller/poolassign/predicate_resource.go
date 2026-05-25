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

package assign

import (
	"context"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

type resourcePredicate struct{}

func newResourcePredicate(_ map[string]interface{}) (Predicate, error) {
	return &resourcePredicate{}, nil
}

func (p *resourcePredicate) Predicate(_ context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) bool {
	if sbx.Spec.Template == nil {
		return true
	}
	sbxResources := aggregateRequests(sbx.Spec.Template.Spec.Containers)
	if len(sbxResources) == 0 {
		return true
	}
	if pool.Spec.Template == nil {
		return false
	}
	poolResources := aggregateRequests(pool.Spec.Template.Spec.Containers)

	for name, req := range sbxResources {
		poolReq, ok := poolResources[name]
		if !ok || poolReq.Cmp(req) < 0 {
			return false
		}
	}
	return true
}

func aggregateRequests(containers []corev1.Container) corev1.ResourceList {
	result := corev1.ResourceList{}
	for _, c := range containers {
		for name, qty := range c.Resources.Requests {
			if existing, ok := result[name]; ok {
				existing.Add(qty)
				result[name] = existing
			} else {
				result[name] = qty.DeepCopy()
			}
		}
	}
	return result
}
