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

func TestLabelSelectorPredicate(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		keys   []string
		sbx    *sandboxv1alpha1.BatchSandbox
		pool   *sandboxv1alpha1.Pool
		expect bool
	}{
		{
			name: "matching labels for all keys",
			keys: []string{"env", "tier"},
			sbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod", "tier": "frontend", "extra": "val"},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod", "tier": "frontend"},
				},
			},
			expect: true,
		},
		{
			name: "mismatched label value",
			keys: []string{"env"},
			sbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod"},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "staging"},
				},
			},
			expect: false,
		},
		{
			name: "sandbox missing required key",
			keys: []string{"env", "tier"},
			sbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod"},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod", "tier": "frontend"},
				},
			},
			expect: false,
		},
		{
			name: "pool missing required key",
			keys: []string{"env"},
			sbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod"},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			expect: false,
		},
		{
			name:   "empty keys - passes",
			keys:   []string{},
			sbx:    &sandboxv1alpha1.BatchSandbox{},
			pool:   &sandboxv1alpha1.Pool{},
			expect: true,
		},
		{
			name:   "nil keys - passes",
			keys:   nil,
			sbx:    &sandboxv1alpha1.BatchSandbox{},
			pool:   &sandboxv1alpha1.Pool{},
			expect: true,
		},
		{
			name: "sandbox nil labels with required keys",
			keys: []string{"env"},
			sbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod"},
				},
			},
			expect: false,
		},
		{
			name: "pool nil labels with required keys",
			keys: []string{"env"},
			sbx: &sandboxv1alpha1.BatchSandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"env": "prod"},
				},
			},
			pool: &sandboxv1alpha1.Pool{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]interface{}{"keys": tt.keys}
			p, err := newLabelSelectorPredicate(args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			got := p.Predicate(ctx, tt.sbx, tt.pool)
			if got != tt.expect {
				t.Errorf("labelSelectorPredicate.Predicate() = %v, want %v", got, tt.expect)
			}
		})
	}
}
