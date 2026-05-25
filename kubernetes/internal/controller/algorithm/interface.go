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

// Algorithm determines how available pods are distributed among sandbox requests.
type Algorithm interface {
	// Schedule distributes available pods among sandbox requests and returns the allocation action.
	Schedule(availablePods []string, allRequest []*SandboxRequest) *AllocAction
}

// SandboxRequest describes a sandbox's allocation need.
type SandboxRequest struct {
	SandboxName   string
	CurAllocation []string
	CurReleased   []string
	PodSupplement int32
	ToRelease     []string
}

// AllocAction represents the result of a scheduling decision.
type AllocAction struct {
	// allocate pods to sandbox (sandbox -> pods)
	ToAllocate map[string][]string
	// release pods from sandbox (sandbox -> pods)
	ToRelease map[string][]string
	// pod request count
	PodSupplement int32
}
