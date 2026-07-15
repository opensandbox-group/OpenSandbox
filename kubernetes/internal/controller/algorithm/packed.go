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

package algorithm

// PackedSchedule allocates pods to each sandbox in order, fully satisfying one before moving to the next.
// This is the default algorithm and provides the simplest packing strategy.
type PackedSchedule struct{}

func (p *PackedSchedule) Schedule(availablePods []string, allRequest []*SandboxRequest) *AllocAction {
	action := &AllocAction{
		ToAllocate:    make(map[string][]string),
		ToRelease:     make(map[string][]string),
		PodSupplement: int32(0),
	}

	for _, req := range allRequest {
		if len(req.ToRelease) > 0 {
			action.ToRelease[req.SandboxName] = req.ToRelease
		}

		need := req.PodSupplement
		if need <= 0 {
			continue
		}
		if int32(len(availablePods)) >= need {
			action.ToAllocate[req.SandboxName] = availablePods[:need]
			availablePods = availablePods[need:]
		} else if len(availablePods) > 0 {
			action.ToAllocate[req.SandboxName] = availablePods
			action.PodSupplement += need - int32(len(availablePods))
			availablePods = nil
		} else {
			action.PodSupplement += need
		}
	}

	return action
}
