// Copyright 2025 The OpenSandbox Authors
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

type KernelSpecs struct {
	Default string `json:"default"`

	Kernelspecs map[string]*KernelSpecInfo `json:"kernelspecs"`
}

type KernelSpecInfo struct {
	Name string `json:"name"`

	Spec KernelSpecDetail `json:"spec"`

	Resources map[string]string `json:"resources,omitempty"`
}

type KernelSpecDetail struct {
	Argv []string `json:"argv,omitempty"`

	DisplayName string `json:"display_name"`

	Language string `json:"language,omitempty"`

	InterruptMode string `json:"interrupt_mode,omitempty"`
}

type Kernel struct {
	ID string `json:"id"`

	Name string `json:"name"`

	LastActivity time.Time `json:"last_activity,omitempty"`

	Connections int `json:"connections,omitempty"`

	ExecutionState string `json:"execution_state,omitempty"`
}

type KernelStartRequest struct {
	Name string `json:"name"`

	Path string `json:"path,omitempty"`
}

type KernelRestartResponse struct {
	ID string `json:"id"`

	Name string `json:"name"`

	Restarted bool `json:"restarted"`

	LastActivity time.Time `json:"last_activity,omitempty"`
}

type KernelInterruptRequest struct {
	Restart bool `json:"restart,omitempty"`
}

type KernelStatus string

const (
	KernelStatusIdle KernelStatus = "idle"

	KernelStatusBusy KernelStatus = "busy"

	KernelStatusStarting KernelStatus = "starting"

	KernelStatusRestarting KernelStatus = "restarting"

	KernelStatusDead KernelStatus = "dead"
)
