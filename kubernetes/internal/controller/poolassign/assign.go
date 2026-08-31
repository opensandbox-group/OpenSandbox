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

// PredicateWithFailureCode exposes a stable identifier for a rejected predicate.
type PredicateWithFailureCode interface {
	Predicate
	FailureCode() string
}

const FailureCodeCapacityExhausted = "PoolCapacityExhausted"

type Scorer interface {
	Score(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) float64
}

type Assigner interface {
	AssignPool(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pools []*sandboxv1alpha1.Pool) (string, error)
}

// PoolRejection records why a specific pool was rejected during assignment.
type PoolRejection struct {
	PoolName     string
	Reasons      []string
	FailureCodes []string
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

// CapacityExhausted reports whether at least one Pool matched every predicate
// except capacity. Pools that also fail image, resource, label, or node
// predicates do not make the assignment a capacity failure.
func (e *NoEligiblePoolError) CapacityExhausted() bool {
	for _, rejection := range e.Rejections {
		if len(rejection.FailureCodes) == 1 &&
			rejection.FailureCodes[0] == FailureCodeCapacityExhausted {
			return true
		}
	}
	return false
}
