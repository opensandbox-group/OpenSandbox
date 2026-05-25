// Copyright 2026 Alibaba Group Holding Ltd.
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

package opensandbox

import "context"

// GetEgressPolicy retrieves the current egress network policy.
func (s *Sandbox) GetEgressPolicy(ctx context.Context) (*PolicyStatusResponse, error) {
	if err := s.resolveEgress(ctx); err != nil {
		return nil, err
	}
	return s.egress.GetPolicy(ctx)
}

// PatchEgressRules merges network rules into the current egress policy.
func (s *Sandbox) PatchEgressRules(ctx context.Context, rules []NetworkRule) (*PolicyStatusResponse, error) {
	if err := s.resolveEgress(ctx); err != nil {
		return nil, err
	}
	return s.egress.PatchPolicy(ctx, rules)
}
