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

type labelSelectorPredicate struct {
	keys []string
}

func newLabelSelectorPredicate(args map[string]interface{}) (Predicate, error) {
	return &labelSelectorPredicate{keys: extractStringSlice(args, "keys")}, nil
}

func extractStringSlice(args map[string]interface{}, key string) []string {
	if args == nil {
		return nil
	}
	raw, ok := args[key]
	if !ok {
		return nil
	}
	if slice, ok := raw.([]string); ok {
		return slice
	}
	slice, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(slice))
	for _, v := range slice {
		if s, ok := v.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

func (p *labelSelectorPredicate) Predicate(_ context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) bool {
	if len(p.keys) == 0 {
		return true
	}
	for _, k := range p.keys {
		sbxVal, sbxOk := sbx.Labels[k]
		if !sbxOk {
			return false
		}
		poolVal, poolOk := pool.Labels[k]
		if !poolOk || poolVal != sbxVal {
			return false
		}
	}
	return true
}
