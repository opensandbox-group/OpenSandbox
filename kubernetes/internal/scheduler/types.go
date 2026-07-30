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

package scheduler

type Task interface {
	GetName() string
	GetState() TaskState
	GetPodName() string
	// IsResourceReleased task resource is released
	// TODO func name is strange
	IsResourceReleased() bool
	// GetTerminatedMessage returns a human-readable message when the task has
	// reached a terminal failure state (e.g., a lifecycle hook error with stderr).
	// Returns an empty string if there is no message or the task is not failed.
	GetTerminatedMessage() string
}

type TaskState string

const (
	RunningTaskState TaskState = "RUNNING"
	FailedTaskState  TaskState = "FAILED"
	SucceedTaskState TaskState = "SUCCEED"
	UnknownTaskState TaskState = "UNKNOWN"
)
