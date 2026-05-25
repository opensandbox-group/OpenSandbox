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

type imagePredicate struct{}

func newImagePredicate(_ map[string]interface{}) (Predicate, error) {
	return &imagePredicate{}, nil
}

func (p *imagePredicate) Predicate(_ context.Context, sbx *sandboxv1alpha1.BatchSandbox, pool *sandboxv1alpha1.Pool) bool {
	if sbx.Spec.Template == nil {
		return true
	}
	sbxImages := make(map[string]struct{})
	for _, c := range sbx.Spec.Template.Spec.Containers {
		if c.Image != "" {
			sbxImages[c.Image] = struct{}{}
		}
	}
	if len(sbxImages) == 0 {
		return true
	}
	if pool.Spec.Template == nil {
		return false
	}
	for _, c := range pool.Spec.Template.Spec.Containers {
		if c.Image == "" {
			continue
		}
		if _, ok := sbxImages[c.Image]; ok {
			return true
		}
	}
	return false
}
