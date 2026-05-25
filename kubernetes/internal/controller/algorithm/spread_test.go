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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSpreadSchedule(t *testing.T) {
	tests := []struct {
		name           string
		availablePods  []string
		allRequest     []*SandboxRequest
		wantAllocate   map[string][]string
		wantRelease    map[string][]string
		wantSupplement int32
	}{
		{
			name:          "AllocateEvenly",
			availablePods: []string{"pod1", "pod2", "pod3", "pod4"},
			allRequest: []*SandboxRequest{
				{SandboxName: "sbx1", PodSupplement: 2},
				{SandboxName: "sbx2", PodSupplement: 2},
			},
			wantAllocate:   map[string][]string{"sbx1": {"pod1", "pod3"}, "sbx2": {"pod2", "pod4"}},
			wantRelease:    map[string][]string{},
			wantSupplement: 0,
		},
		{
			name:          "UnevenNeed",
			availablePods: []string{"pod1", "pod2", "pod3"},
			allRequest: []*SandboxRequest{
				{SandboxName: "sbx1", PodSupplement: 1},
				{SandboxName: "sbx2", PodSupplement: 2},
			},
			wantAllocate:   map[string][]string{"sbx1": {"pod1"}, "sbx2": {"pod2", "pod3"}},
			wantRelease:    map[string][]string{},
			wantSupplement: 0,
		},
		{
			name:          "NotEnoughPods",
			availablePods: []string{"pod1"},
			allRequest: []*SandboxRequest{
				{SandboxName: "sbx1", PodSupplement: 2},
			},
			wantAllocate:   map[string][]string{"sbx1": {"pod1"}},
			wantRelease:    map[string][]string{},
			wantSupplement: 1,
		},
		{
			name:          "WithRelease",
			availablePods: []string{"pod3"},
			allRequest: []*SandboxRequest{
				{SandboxName: "sbx1", PodSupplement: 1, ToRelease: []string{"pod1"}},
			},
			wantAllocate:   map[string][]string{"sbx1": {"pod3"}},
			wantRelease:    map[string][]string{"sbx1": {"pod1"}},
			wantSupplement: 0,
		},
		{
			name:          "NoAvailablePods",
			availablePods: nil,
			allRequest: []*SandboxRequest{
				{SandboxName: "sbx1", PodSupplement: 3},
				{SandboxName: "sbx2", PodSupplement: 2},
			},
			wantAllocate:   map[string][]string{},
			wantRelease:    map[string][]string{},
			wantSupplement: 5,
		},
		{
			name:           "EmptyRequests",
			availablePods:  []string{"pod1"},
			allRequest:     nil,
			wantAllocate:   map[string][]string{},
			wantRelease:    map[string][]string{},
			wantSupplement: 0,
		},
		{
			name:          "SpreadAllocation",
			availablePods: []string{"pod1", "pod2", "pod3", "pod4", "pod5"},
			allRequest: []*SandboxRequest{
				{SandboxName: "sbx1", PodSupplement: 3},
				{SandboxName: "sbx2", PodSupplement: 2},
				{SandboxName: "sbx3", PodSupplement: 1},
			},
			// Round 1: sbx1→pod1, sbx2→pod2, sbx3→pod3
			// Round 2: sbx1→pod4, sbx2→pod5
			wantAllocate:   map[string][]string{"sbx1": {"pod1", "pod4"}, "sbx2": {"pod2", "pod5"}, "sbx3": {"pod3"}},
			wantRelease:    map[string][]string{},
			wantSupplement: 1, // 1 remaining for sbx1
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := (&SpreadSchedule{}).Schedule(tt.availablePods, tt.allRequest)
			assert.Equal(t, tt.wantAllocate, action.ToAllocate)
			assert.Equal(t, tt.wantRelease, action.ToRelease)
			assert.Equal(t, tt.wantSupplement, action.PodSupplement)
		})
	}
}
