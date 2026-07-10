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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

func int32Ptr(v int32) *int32 { return &v }

func TestCapacityPredicate(t *testing.T) {
	p, err := newCapacityPredicate(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctx := context.Background()

	tests := []struct {
		name      string
		replicas  *int32
		poolMax   int32
		allocated int32
		expect    bool
	}{
		{
			name:      "nil replicas defaults to 1 - pool has headroom",
			replicas:  nil,
			poolMax:   10,
			allocated: 5,
			expect:    true,
		},
		{
			name:      "nil replicas defaults to 1 - pool full",
			replicas:  nil,
			poolMax:   10,
			allocated: 10,
			expect:    false,
		},
		{
			name:      "replicas fit exactly in headroom",
			replicas:  int32Ptr(3),
			poolMax:   10,
			allocated: 7,
			expect:    true,
		},
		{
			name:      "replicas exceed headroom by one",
			replicas:  int32Ptr(3),
			poolMax:   10,
			allocated: 8,
			expect:    false,
		},
		{
			name:      "replicas exceed PoolMax outright",
			replicas:  int32Ptr(5),
			poolMax:   3,
			allocated: 0,
			expect:    false,
		},
		{
			name:      "scale-from-zero pool can accept replicas within PoolMax",
			replicas:  int32Ptr(3),
			poolMax:   5,
			allocated: 0,
			expect:    true,
		},
		{
			name:      "zero PoolMax pool always ineligible",
			replicas:  int32Ptr(1),
			poolMax:   0,
			allocated: 0,
			expect:    false,
		},
		{
			name:      "zero replicas is trivially satisfied",
			replicas:  int32Ptr(0),
			poolMax:   1,
			allocated: 1,
			expect:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx-1"},
				Spec:       sandboxv1alpha1.BatchSandboxSpec{Replicas: tt.replicas},
			}
			pool := &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool-test"},
				Spec: sandboxv1alpha1.PoolSpec{
					CapacitySpec: sandboxv1alpha1.CapacitySpec{PoolMax: tt.poolMax},
				},
				Status: sandboxv1alpha1.PoolStatus{Allocated: tt.allocated},
			}
			got := p.Predicate(ctx, sbx, pool)
			if got != tt.expect {
				t.Errorf("capacityPredicate.Predicate() = %v, want %v (poolMax=%d, allocated=%d, replicas=%v)",
					got, tt.expect, tt.poolMax, tt.allocated, tt.replicas)
			}
		})
	}
}
