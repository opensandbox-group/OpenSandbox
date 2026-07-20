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

package types

import (
	"time"

	corev1 "k8s.io/api/core/v1"

	api "github.com/alibaba/OpenSandbox/sandbox-k8s/pkg/task-executor"
)

// TaskState defines the simplified internal state of a task.
type TaskState string

const (
	TaskStatePending   TaskState = "Pending"
	TaskStateRunning   TaskState = "Running"
	TaskStateSucceeded TaskState = "Succeeded"
	TaskStateFailed    TaskState = "Failed"
	TaskStateUnknown   TaskState = "Unknown"
	TaskStateNotFound  TaskState = "NotFound"
	TaskStateTimeout   TaskState = "Timeout"
)

// Status represents the internal status of a task.
// This is decoupled from the Kubernetes API status.
type Status struct {
	State       TaskState   `json:"state"`
	SubStatuses []SubStatus `json:"subStatuses,omitempty"`
}

type SubStatus struct {
	Name       string     `json:"name,omitempty"` // for process it's empty, for PodTemplateSpec is container name
	Reason     string     `json:"reason,omitempty"`
	Message    string     `json:"message,omitempty"`
	ExitCode   int        `json:"exitCode,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

type Task struct {
	Name              string     `json:"name"`
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty"`

	Process         *api.Process            `json:"process"`
	PodTemplateSpec *corev1.PodTemplateSpec `json:"podTemplateSpec"`

	// Status is now a first-class citizen and persisted.
	Status Status `json:"status"`
}

// StartError is returned by Executor.Start() to carry a structured Reason so
// that the manager can persist an accurate SubStatus without guessing.
type StartError struct {
	// Reason is a machine-readable token such as "PreStartHookFailed" or
	// "ProcessStartFailed".
	Reason string
	// Message is a human-readable description, typically including stderr.
	Message string
}

func (e *StartError) Error() string {
	return e.Message
}

// Predefined Reason constants for StartError.
const (
	ReasonPreStartHookFailed = "PreStartHookFailed"
	ReasonProcessStartFailed = "ProcessStartFailed"
)

// StopError is returned by Executor.Stop() to carry a structured Reason so
// that the manager can persist an accurate SubStatus without guessing.
type StopError struct {
	// Reason is a machine-readable token such as "ProcessStopFailed" or
	// "PostStopHookFailed".
	Reason string
	// Message is a human-readable description, typically including stderr.
	Message string
}

func (e *StopError) Error() string {
	return e.Message
}

// Predefined Reason constants for StopError.
const (
	ReasonProcessStopFailed  = "ProcessStopFailed"
	ReasonPostStopHookFailed = "PostStopHookFailed"
)
