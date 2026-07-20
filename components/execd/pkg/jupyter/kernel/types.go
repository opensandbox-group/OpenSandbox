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

// Package kernel provides functionality for managing Jupyter kernels
package kernel

import (
	"time"
)

// KernelSpecs contains available kernel specification information
type KernelSpecs struct {
	// Default is the name of the default kernel
	Default string `json:"default"`

	// Kernelspecs is a mapping from kernel names to kernel specifications
	Kernelspecs map[string]*KernelSpecInfo `json:"kernelspecs"`
}

// KernelSpecInfo contains detailed kernel specification information
type KernelSpecInfo struct {
	// Name is the name of the kernel
	Name string `json:"name"`

	Spec KernelSpecDetail `json:"spec"`

	// Resources contains resource paths related to the kernel
	Resources map[string]string `json:"resources,omitempty"`
}

type KernelSpecDetail struct {
	Argv []string `json:"argv,omitempty"`

	// DisplayName is the display name of the kernel
	DisplayName string `json:"display_name"`

	// Language is the programming language used by the kernel
	Language string `json:"language,omitempty"`

	// InterruptMode is the interrupt mode of the kernel
	InterruptMode string `json:"interrupt_mode,omitempty"`
}

// Kernel represents a running kernel instance
type Kernel struct {
	// ID is the unique identifier of the kernel
	ID string `json:"id"`

	// Name is the name of the kernel
	Name string `json:"name"`

	// LastActivity is the timestamp of the kernel's last activity
	LastActivity time.Time `json:"last_activity,omitempty"`

	// Connections is the number of clients currently connected to the kernel
	Connections int `json:"connections,omitempty"`

	// ExecutionState is the execution state of the kernel (e.g., idle, busy)
	ExecutionState string `json:"execution_state,omitempty"`
}

// KernelStartRequest is the request for starting a new kernel
type KernelStartRequest struct {
	// Name is the name of the kernel to start
	Name string `json:"name"`

	// Path is the optional path for the kernel
	Path string `json:"path,omitempty"`
}

// KernelRestartResponse representsresponse of kernel restart
type KernelRestartResponse struct {
	// ID is the ID of the restarted kernel
	ID string `json:"id"`

	// Name is the restarted kernel name
	Name string `json:"name"`

	// Restarted represents whether the kernel was successfully restarted
	Restarted bool `json:"restarted"`

	// LastActivity is the timestamp of the kernel's last activity
	LastActivity time.Time `json:"last_activity,omitempty"`
}

// KernelInterruptRequest request to interrupt a kernel
type KernelInterruptRequest struct {
	// Restart represents whether to restart the kernel after interruption
	Restart bool `json:"restart,omitempty"`
}

// KernelStatus represents the status of the kernel
type KernelStatus string

const (
	// KernelStatusIdle representskernel is idle
	KernelStatusIdle KernelStatus = "idle"

	// KernelStatusBusy representskernel is busy
	KernelStatusBusy KernelStatus = "busy"

	// KernelStatusStarting representskernel is starting
	KernelStatusStarting KernelStatus = "starting"

	// KernelStatusRestarting represents the kernel is restarting
	KernelStatusRestarting KernelStatus = "restarting"

	// KernelStatusDead represents the kernel is dead
	KernelStatusDead KernelStatus = "dead"
)
