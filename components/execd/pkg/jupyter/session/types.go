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

// Package session provides functionality for managing Jupyter sessions
package session

import (
	"time"
)

// Session represents a Jupyter session
type Session struct {
	// ID is the unique identifier of the session
	ID string `json:"id"`

	// Path is the path associated with the session (typically the notebook file path)
	Path string `json:"path"`

	// Name is the name of the session
	Name string `json:"name"`

	// Type is the type of the session (e.g., notebook, console)
	Type string `json:"type"`

	// Kernel contains information about the kernel associated with the session
	Kernel *KernelInfo `json:"kernel"`

	// CreatedAt is the timestamp when the session was created
	CreatedAt time.Time `json:"created,omitempty"`

	// LastModified is the timestamp when the session was last modified
	LastModified time.Time `json:"last_modified,omitempty"`
}

// KernelInfo contains basic kernel information
type KernelInfo struct {
	// ID is the unique identifier of the kernel
	ID string `json:"id"`

	// Name is the name of the kernel (e.g., python3, ir)
	Name string `json:"name"`

	// LastActivity is the timestamp of the kernel's last activity
	LastActivity time.Time `json:"last_activity,omitempty"`

	// Connections is the number of clients currently connected to the kernel
	Connections int `json:"connections,omitempty"`

	// ExecutionState is the execution state of the kernel (e.g., idle, busy)
	ExecutionState string `json:"execution_state,omitempty"`
}

// SessionCreateRequest is the request for creating a new session
type SessionCreateRequest struct {
	// Path is the path associated with the session (typically the notebook file path)
	Path string `json:"path"`

	// Name is the name of the session
	Name string `json:"name,omitempty"`

	// Type is the type of the session (defaults to "notebook")
	Type string `json:"type,omitempty"`

	// Kernel contains information about the kernel to start
	Kernel *KernelSpec `json:"kernel,omitempty"`
}

// KernelSpec contains kernel specification information
type KernelSpec struct {
	// Name is the name of the kernel (e.g., python3, ir)
	Name string `json:"name"`

	// ID is the unique identifier of the kernel (optional, used only when reusing existing kernel)
	ID string `json:"id,omitempty"`
}

// SessionListResponse represents the response for listing sessions
type SessionListResponse []*Session

// SessionOptions contains options for creating or updating sessions
type SessionOptions struct {
	// Name is the name of the session
	Name string

	// Path is the path associated with the session
	Path string

	// Type is the type of the session (defaults to "notebook")
	Type string

	// KernelName is the kernel name to use (e.g., python3, ir, etc.)
	KernelName string

	// KernelID is the ID of the existing kernel to reuse (if provided, KernelName will be ignored)
	KernelID string
}

// DefaultSessionType is the default session type
const DefaultSessionType = "notebook"
