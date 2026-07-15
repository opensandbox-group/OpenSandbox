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

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

type capacityPredicate struct{}

func newCapacityPredicate(_ map[string]interface{}) (Predicate, error) {
	return &capacityPredicate{}, nil
}

func (p *capacityPredicate) Predicate(_ context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) bool {
	desired := int32(1)
	if sbx.Spec.Replicas != nil {
		desired = *sbx.Spec.Replicas
	}
	return pool.Spec.CapacitySpec.PoolMax-pool.Status.Allocated >= desired
}
