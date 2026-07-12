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
	"fmt"
	"strings"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

type Predicate interface {
	Predicate(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) bool
}

// PredicateWithReason extends Predicate with rejection diagnostics.
type PredicateWithReason interface {
	Predicate
	Reason(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) string
}

type Scorer interface {
	Score(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) float64
}

type Assigner interface {
	AssignPool(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pools []*sandboxv1alpha1.Pool) (string, error)
}

// PoolRejection records why a specific pool was rejected during assignment.
type PoolRejection struct {
	PoolName string
	Reasons  []string
}

// NoEligiblePoolError is returned when no pool passes all predicates.
type NoEligiblePoolError struct {
	SandboxName string
	TotalPools  int
	Rejections  []PoolRejection
}

func (e *NoEligiblePoolError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "0/%d pools are available:", e.TotalPools)
	for _, r := range e.Rejections {
		fmt.Fprintf(&sb, "\n  %s: %s", r.PoolName, strings.Join(r.Reasons, "; "))
	}
	return sb.String()
}
