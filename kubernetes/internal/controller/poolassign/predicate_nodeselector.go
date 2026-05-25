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
	"strconv"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "github.com/alibaba/OpenSandbox/sandbox-k8s/apis/sandbox/v1alpha1"
)

type nodeSelectorPredicate struct{}

func newNodeSelectorPredicate(_ map[string]interface{}) (Predicate, error) {
	return &nodeSelectorPredicate{}, nil
}

func (p *nodeSelectorPredicate) Predicate(_ context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) bool {
	if sbx.Spec.Template == nil {
		return true
	}

	poolLabels := mergePoolLabels(pool)

	if !nodeSelectorMatch(sbx.Spec.Template.Spec.NodeSelector, poolLabels) {
		return false
	}

	if !nodeAffinityMatch(sbx.Spec.Template.Spec.Affinity, poolLabels) {
		return false
	}

	return true
}

func mergePoolLabels(pool *sandboxv1alpha1.Pool) map[string]string {
	result := make(map[string]string, len(pool.Labels))
	for k, v := range pool.Labels {
		result[k] = v
	}
	if pool.Spec.Template != nil {
		for k, v := range pool.Spec.Template.Spec.NodeSelector {
			result[k] = v
		}
	}
	return result
}

func nodeSelectorMatch(sbxSelector, poolSelector map[string]string) bool {
	for k, v := range sbxSelector {
		if poolV, ok := poolSelector[k]; !ok || poolV != v {
			return false
		}
	}
	return true
}

func nodeAffinityMatch(affinity *corev1.Affinity, poolLabels map[string]string) bool {
	if affinity == nil || affinity.NodeAffinity == nil {
		return true
	}
	req := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if req == nil || len(req.NodeSelectorTerms) == 0 {
		return true
	}

	for _, term := range req.NodeSelectorTerms {
		if termMatchExpressions(term.MatchExpressions, poolLabels) {
			return true
		}
	}
	return false
}

func termMatchExpressions(exprs []corev1.NodeSelectorRequirement, labels map[string]string) bool {
	for _, expr := range exprs {
		val, hasVal := labels[expr.Key]
		switch expr.Operator {
		case corev1.NodeSelectorOpIn:
			if !hasVal || !valueInList(val, expr.Values) {
				return false
			}
		case corev1.NodeSelectorOpNotIn:
			if hasVal && valueInList(val, expr.Values) {
				return false
			}
		case corev1.NodeSelectorOpExists:
			if !hasVal {
				return false
			}
		case corev1.NodeSelectorOpDoesNotExist:
			if hasVal {
				return false
			}
		case corev1.NodeSelectorOpGt:
			if !hasVal || !cmpIntGt(val, expr.Values) {
				return false
			}
		case corev1.NodeSelectorOpLt:
			if !hasVal || !cmpIntLt(val, expr.Values) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func cmpIntGt(labelVal string, values []string) bool {
	if len(values) == 0 {
		return false
	}
	lv, err := strconv.Atoi(labelVal)
	if err != nil {
		return false
	}
	rv, err := strconv.Atoi(values[0])
	if err != nil {
		return false
	}
	return lv > rv
}

func cmpIntLt(labelVal string, values []string) bool {
	if len(values) == 0 {
		return false
	}
	lv, err := strconv.Atoi(labelVal)
	if err != nil {
		return false
	}
	rv, err := strconv.Atoi(values[0])
	if err != nil {
		return false
	}
	return lv < rv
}

func valueInList(target string, list []string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}
