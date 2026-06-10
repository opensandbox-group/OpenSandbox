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
	"time"

	"github.com/go-playground/validator/v10"
)

// Create

// CreateIsolatedSessionRequest is the request body for POST /v1/isolated/session.
type CreateIsolatedSessionRequest struct {
	Isolation IsolationSpec `json:"isolation" validate:"required"`
}

// IsolationSpec configures per-session isolation parameters.
type IsolationSpec struct {
	Profile            string             `json:"profile"` // "strict" | "balanced"
	Workspace          WorkspaceSpec      `json:"workspace" validate:"required"`
	ExtraWritable      []string           `json:"extra_writable,omitempty"`
	ShareNet           *bool              `json:"share_net,omitempty"`
	EnvPassthrough     EnvPassthroughSpec `json:"env_passthrough,omitempty"`
	Uid                *uint32            `json:"uid,omitempty"`
	Gid                *uint32            `json:"gid,omitempty"`
	IdleTimeoutSeconds int                `json:"idle_timeout_seconds,omitempty"`
}

// WorkspaceSpec describes the workspace mount.
type WorkspaceSpec struct {
	Path    string       `json:"path" validate:"required"`
	Mode    string       `json:"mode,omitempty"` // "rw" | "overlay" | "ro", default per profile
	Persist *PersistSpec `json:"persist,omitempty"`
}

// PersistSpec controls upper directory persistence for overlay mode.
type PersistSpec struct {
	Enabled       bool `json:"enabled"`
	RetainSeconds int  `json:"retain_seconds,omitempty"`
	MaxSizeBytes  int  `json:"max_size_bytes,omitempty"`
}

// EnvPassthroughSpec controls environment passthrough into the namespace.
type EnvPassthroughSpec struct {
	Mode string   `json:"mode,omitempty"` // "deny" | "allow"
	Keys []string `json:"keys,omitempty"`
}

// IsolatedCreateSessionResponse is the response for POST /v1/isolated/session.
type IsolatedCreateSessionResponse struct {
	SessionID string        `json:"session_id"`
	CreatedAt time.Time     `json:"created_at"`
	Isolation IsolationSpec `json:"isolation"`
	Artifacts *ArtifactURLs `json:"artifacts,omitempty"`
}

// ArtifactURLs exposes diff/commit endpoints when persist is enabled.
type ArtifactURLs struct {
	DiffURL   string `json:"diff_url"`
	CommitURL string `json:"commit_url"`
}

// Validate checks CreateIsolatedSessionRequest fields.
func (r *CreateIsolatedSessionRequest) Validate() error {
	v := validator.New()
	return v.Struct(r)
}

// Run

// IsolatedRunRequest is the request body for POST /v1/isolated/session/<id>/run.
type IsolatedRunRequest struct {
	Code           string            `json:"code" validate:"required"`
	Cwd            string            `json:"cwd,omitempty"`
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
	Status               string        `json:"status"` // "active" | "destroyed"
	CreatedAt            time.Time     `json:"created_at"`
	LastRunAt            time.Time     `json:"last_run_at"`
	IdleRemainingSeconds *int          `json:"idle_remaining_seconds,omitempty"`
	Isolation            IsolationSpec `json:"isolation"`
}

// Capabilities

// CapabilitiesResponse is returned by GET /v1/isolated/capabilities.
type CapabilitiesResponse struct {
	Available       bool   `json:"available"`
	Isolator        string `json:"isolator,omitempty"`
	Version         string `json:"version,omitempty"`
	CommitSupported bool   `json:"commit_supported"`
	DiffSupported   bool   `json:"diff_supported"`
}
