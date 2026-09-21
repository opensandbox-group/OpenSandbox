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

// Package session provides functionality for managing Jupyter sessions
package session

import (
	"time"
)

type Session struct {
	ID string `json:"id"`

	Path string `json:"path"`

	Name string `json:"name"`

	Type string `json:"type"`

	Kernel *KernelInfo `json:"kernel"`

	CreatedAt time.Time `json:"created,omitempty"`

	LastModified time.Time `json:"last_modified,omitempty"`
}

type KernelInfo struct {
	ID string `json:"id"`

	Name string `json:"name"`

	LastActivity time.Time `json:"last_activity,omitempty"`

	Connections int `json:"connections,omitempty"`

	ExecutionState string `json:"execution_state,omitempty"`
}

type SessionCreateRequest struct {
	Path string `json:"path"`

	Name string `json:"name,omitempty"`

	// Type is the type of the session (defaults to "notebook")
	Type string `json:"type,omitempty"`

	Kernel *KernelSpec `json:"kernel,omitempty"`
}

type KernelSpec struct {
	Name string `json:"name"`

	// ID is the unique identifier of the kernel (optional, used only when reusing existing kernel)
	ID string `json:"id,omitempty"`
}

type SessionListResponse []*Session

type SessionOptions struct {
	Name string

	Path string

	// Type is the type of the session (defaults to "notebook")
	Type string

	KernelName string

	// KernelID is the ID of the existing kernel to reuse (if provided, KernelName will be ignored)
	KernelID string
}

const DefaultSessionType = "notebook"
