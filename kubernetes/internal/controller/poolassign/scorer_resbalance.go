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

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

const (
	strategyMostAllocated  = "MostAllocated"
	strategyLeastAllocated = "LeastAllocated"
)

type resBalanceScorer struct {
	strategy string
}

func newResBalanceScorer(args map[string]interface{}) (Scorer, error) {
	strategy := extractStrategy(args)
	switch strategy {
	case strategyMostAllocated, strategyLeastAllocated:
	default:
		return nil, fmt.Errorf("unknown strategy %q, must be %q or %q", strategy, strategyMostAllocated, strategyLeastAllocated)
	}
	return &resBalanceScorer{strategy: strategy}, nil
}

func extractStrategy(args map[string]interface{}) string {
	if s, ok := args["strategy"].(string); ok && s != "" {
		return s
	}
	return strategyLeastAllocated
}

func (s *resBalanceScorer) Score(_ context.Context, _ *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) float64 {
	if pool.Status.Total == 0 {
		return 0
	}
	ratio := float64(pool.Status.Allocated) / float64(pool.Status.Total)
	if s.strategy == strategyLeastAllocated {
		return 1 - ratio
	}
	return ratio
}
