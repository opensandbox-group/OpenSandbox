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

// SpreadSchedule distributes pods evenly across sandboxes in a round-robin fashion,
// like water filling — each sandbox gets one pod per round until its need is met.
type SpreadSchedule struct{}

func (s *SpreadSchedule) Schedule(availablePods []string, allRequest []*SandboxRequest) *AllocAction {
	action := &AllocAction{
		ToAllocate:    make(map[string][]string),
		ToRelease:     make(map[string][]string),
		PodSupplement: int32(0),
	}

	// Process ToRelease first.
	for _, req := range allRequest {
		if len(req.ToRelease) > 0 {
			action.ToRelease[req.SandboxName] = req.ToRelease
		}
	}

	// Build a list of sandboxes that still need pods, with their remaining counts.
	type needEntry struct {
		sandboxName string
		remaining   int32
	}
	var needs []needEntry
	for _, req := range allRequest {
		if req.PodSupplement > 0 {
			needs = append(needs, needEntry{sandboxName: req.SandboxName, remaining: req.PodSupplement})
		}
	}

	// Round-robin: each round give one pod to each sandbox that still needs pods.
	podIdx := 0
	for podIdx < len(availablePods) && len(needs) > 0 {
		var nextRound []needEntry
		for _, n := range needs {
			if podIdx >= len(availablePods) {
				break
			}
			action.ToAllocate[n.sandboxName] = append(action.ToAllocate[n.sandboxName], availablePods[podIdx])
			podIdx++
			n.remaining--
			if n.remaining > 0 {
				nextRound = append(nextRound, n)
			}
		}
		needs = nextRound
	}

	// Calculate PodSupplement: any remaining unmet need across all sandboxes.
	for _, n := range needs {
		action.PodSupplement += n.remaining
	}

	return action
}
