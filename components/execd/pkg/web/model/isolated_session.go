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

package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

// Workspace mode values.
const (
	WorkspaceModeRW      = "rw"
	WorkspaceModeOverlay = "overlay"
	WorkspaceModeRO      = "ro"
)

// Create

// CreateIsolatedSessionRequest is the request body for POST /v1/isolated/session.
type CreateIsolatedSessionRequest struct {
	Profile            string             `json:"profile"` // "strict" | "balanced"
	Workspace          WorkspaceSpec      `json:"workspace" validate:"required"`
	ExtraWritable      []string           `json:"extra_writable,omitempty"`
	Binds              []BindMount        `json:"binds,omitempty"`
	ShareNet           *bool              `json:"share_net,omitempty"`
	EnvPassthrough     EnvPassthroughSpec `json:"env_passthrough,omitempty"`
	Uid                *uint32            `json:"uid,omitempty"`
	Gid                *uint32            `json:"gid,omitempty"`
	UidMode            string             `json:"uid_mode,omitempty"` // "setpriv" (default) | "userns"
	IdleTimeoutSeconds int                `json:"idle_timeout_seconds,omitempty"`
}

// WorkspaceSpec describes the workspace mount.
type WorkspaceSpec struct {
	Path string `json:"path" validate:"required"`
	Mode string `json:"mode,omitempty"` // "rw" | "overlay" | "ro", default per profile
}

// EnvPassthroughSpec controls environment passthrough into the namespace.
type EnvPassthroughSpec struct {
	Mode string   `json:"mode,omitempty"` // "deny" | "allow"
	Keys []string `json:"keys,omitempty"`
}

// BindMount describes an explicit source→dest bind mount into the namespace.
type BindMount struct {
	Source   string `json:"source" validate:"required"`
	Dest     string `json:"dest,omitempty"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

// IsolatedCreateSessionResponse is the response for POST /v1/isolated/session.
type IsolatedCreateSessionResponse struct {
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Validate checks CreateIsolatedSessionRequest fields.
func (r *CreateIsolatedSessionRequest) Validate() error {
	v := validator.New()
	if err := v.Struct(r); err != nil {
		return err
	}
	if r.Workspace.Mode != "" {
		switch r.Workspace.Mode {
		case WorkspaceModeRW, WorkspaceModeOverlay, WorkspaceModeRO:
		default:
			return fmt.Errorf("invalid workspace mode %q: must be %s, %s, or %s",
				r.Workspace.Mode, WorkspaceModeRW, WorkspaceModeOverlay, WorkspaceModeRO)
		}
	}
	if r.EnvPassthrough.Mode != "" {
		switch r.EnvPassthrough.Mode {
		case "deny", "allow":
		default:
			return fmt.Errorf("invalid env_passthrough mode %q: must be \"deny\" or \"allow\"",
				r.EnvPassthrough.Mode)
		}
	}
	if r.UidMode != "" {
		switch r.UidMode {
		case "setpriv", "userns":
		default:
			return fmt.Errorf("invalid uid_mode %q: must be \"setpriv\" or \"userns\"",
				r.UidMode)
		}
	}
	for i, b := range r.Binds {
		if b.Source == "" {
			return fmt.Errorf("binds[%d].source is required", i)
		}
		if !strings.HasPrefix(b.Source, "/") {
			return fmt.Errorf("binds[%d].source %q must be an absolute path", i, b.Source)
		}
		if b.Dest != "" && !strings.HasPrefix(b.Dest, "/") {
			return fmt.Errorf("binds[%d].dest %q must be an absolute path", i, b.Dest)
		}
	}
	return nil
}

// Run

// IsolatedRunRequest is the request body for POST /v1/isolated/session/<id>/run.
type IsolatedRunRequest struct {
	Code           string            `json:"code" validate:"required"`
	Envs           map[string]string `json:"envs,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" validate:"omitempty,gte=0"`
}

// Validate checks IsolatedRunRequest fields.
func (r *IsolatedRunRequest) Validate() error {
	v := validator.New()
	return v.Struct(r)
}

// Session State

// SessionState is returned by GET /v1/isolated/session/<id>.
type SessionState struct {
	Status               string    `json:"status"` // "active" | "destroyed"
	CreatedAt            time.Time `json:"created_at"`
	LastRunAt            time.Time `json:"last_run_at"`
	IdleRemainingSeconds *int      `json:"idle_remaining_seconds,omitempty"`
}

// IsolatedSessionSummary describes a single session in a list response.
type IsolatedSessionSummary struct {
	SessionID            string    `json:"session_id"`
	Status               string    `json:"status"` // "active" | "dead"
	CreatedAt            time.Time `json:"created_at"`
	LastRunAt            time.Time `json:"last_run_at"`
	IdleRemainingSeconds *int      `json:"idle_remaining_seconds,omitempty"`
}

// ListIsolatedSessionsResponse is returned by GET /v1/isolated/sessions.
type ListIsolatedSessionsResponse struct {
	Sessions []IsolatedSessionSummary `json:"sessions"`
}

// Capabilities

// CapabilitiesResponse is returned by GET /v1/isolated/capabilities.
type CapabilitiesResponse struct {
	Available       bool   `json:"available"`
	Isolator        string `json:"isolator,omitempty"`
	Version         string `json:"version,omitempty"`
	Message         string `json:"message,omitempty"`
	CommitSupported bool   `json:"commit_supported"`
	DiffSupported   bool   `json:"diff_supported"`
}
