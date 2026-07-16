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

package controller

// Event reasons for BatchSandbox and Pool controllers.
const (
	// Pod lifecycle (used by both BatchSandbox and Pool controllers)
	EventReasonFailedCreate     = "FailedCreate"
	EventReasonSuccessfulCreate = "SuccessfulCreate"
	EventReasonFailedDelete     = "FailedDelete"
	EventReasonSuccessfulDelete = "SuccessfulDelete"

	// Pool allocation — recorded on BatchSandbox by pool-controller
	EventReasonScheduled = "Scheduled"

	// Pool assignment — recorded on BatchSandbox by batchsandbox-controller
	EventReasonPoolAssigned     = "PoolAssigned"
	EventReasonFailedPoolAssign = "FailedPoolAssign"

	// Pod release — recorded on BatchSandbox
	EventReasonPodReleased   = "PodReleased"
	EventReasonFailedRelease = "FailedRelease"

	// Pod eviction — recorded on Pool
	EventReasonPodEvicted = "PodEvicted"

	// Rolling update — recorded on Pool
	EventReasonPodUpdated = "PodUpdated"

	// Allocation result — recorded on Pool
	EventReasonAllocationSucceeded = "AllocationSucceeded"
	EventReasonAllocationFailed    = "AllocationFailed"

	// Pod recycle — recorded on Pool
	EventReasonPodRecycled      = "PodRecycled"
	EventReasonFailedRecyclePod = "FailedRecyclePod"
)
