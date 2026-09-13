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
	eventReasonFailedCreate     = "FailedCreate"
	eventReasonSuccessfulCreate = "SuccessfulCreate"
	eventReasonFailedDelete     = "FailedDelete"
	eventReasonSuccessfulDelete = "SuccessfulDelete"

	// Pool allocation — recorded on BatchSandbox by pool-controller
	eventReasonScheduled = "Scheduled"

	// Pool assignment — recorded on BatchSandbox by batchsandbox-controller
	eventReasonPoolAssigned     = "PoolAssigned"
	eventReasonFailedPoolAssign = "FailedPoolAssign"

	// Pod release — recorded on BatchSandbox
	eventReasonPodReleased   = "PodReleased"
	eventReasonFailedRelease = "FailedRelease"

	// Pod eviction — recorded on Pool
	eventReasonPodEvicted = "PodEvicted"

	// Rolling update — recorded on Pool
	eventReasonPodUpdated = "PodUpdated"

	// Allocation result — recorded on Pool
	eventReasonAllocationSucceeded = "AllocationSucceeded"
	eventReasonAllocationFailed    = "AllocationFailed"

	// Pod recycle — recorded on Pool
	eventReasonPodRecycled      = "PodRecycled"
	eventReasonFailedRecyclePod = "FailedRecyclePod"
)
