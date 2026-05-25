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

type defaultAssigner struct {
	profile *Profile
}

func NewDefaultAssigner(profile *Profile) Assigner {
	return &defaultAssigner{profile: profile}
}

func (a *defaultAssigner) AssignPool(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pools []*sandboxv1alpha1.Pool) (string, error) {
	predicates, err := NewPredicates(a.profile)
	if err != nil {
		return "", fmt.Errorf("failed to create predicates: %w", err)
	}
	scorers, err := NewScorers(a.profile)
	if err != nil {
		return "", fmt.Errorf("failed to create scorers: %w", err)
	}

	var candidates []*sandboxv1alpha1.Pool
	for _, pool := range pools {
		if a.passesAllPredicates(ctx, sbx, pool, predicates) {
			candidates = append(candidates, pool)
		}
	}

	if len(candidates) == 0 {
		return "", fmt.Errorf("no eligible pool found for BatchSandbox %s", sbx.Name)
	}

	best := candidates[0]
	bestScore := a.weightedScore(ctx, sbx, best, scorers)
	for _, pool := range candidates[1:] {
		score := a.weightedScore(ctx, sbx, pool, scorers)
		if score > bestScore || (score == bestScore && pool.Name < best.Name) {
			best = pool
			bestScore = score
		}
	}
	return best.Name, nil
}

func (a *defaultAssigner) passesAllPredicates(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool, predicates []Predicate) bool {
	for _, p := range predicates {
		if !p.Predicate(ctx, sbx, pool) {
			return false
		}
	}
	return true
}

func (a *defaultAssigner) weightedScore(ctx context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool, scorers []weightedScorer) float64 {
	var total float64
	for _, s := range scorers {
		total += s.Score(ctx, sbx, pool) * float64(s.weight)
	}
	return total
}
