// Copyright 2026 The OpenSandbox Authors
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
	"path/filepath"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

const (
	WorkspaceModeRW      = "rw"
	WorkspaceModeOverlay = "overlay"
	WorkspaceModeRO      = "ro"
)

// Create

type CreateIsolatedSessionRequest struct {
	Profile string `json:"profile"` // "strict" | "balanced"
	// Workspace is the legacy single-workspace sugar: when set it is
	// prepended to Overlays. At least one of Workspace/Overlays is required.
	Workspace          *WorkspaceSpec     `json:"workspace,omitempty"`
	Overlays           []OverlaySpec      `json:"overlays,omitempty"`
	ExtraWritable      []string           `json:"extra_writable,omitempty"`
	Binds              []BindMount        `json:"binds,omitempty"`
	ShareNet           *bool              `json:"share_net,omitempty"`
	EnvPassthrough     EnvPassthroughSpec `json:"env_passthrough,omitempty"`
	Uid                *uint32            `json:"uid,omitempty"`
	Gid                *uint32            `json:"gid,omitempty"`
	UidMode            string             `json:"uid_mode,omitempty"` // "setpriv" (default) | "userns"
	IdleTimeoutSeconds int                `json:"idle_timeout_seconds,omitempty"`
}

type WorkspaceSpec struct {
	Path string `json:"path" validate:"required"`
	Mode string `json:"mode,omitempty"` // "rw" | "overlay" | "ro", default per profile
}

// OverlaySpec is one overlay mount inside the isolated namespace. Mode
// defaults to overlay; Persist defaults to true and applies to overlay mode
// only (false selects the ephemeral tmpfs upper).
type OverlaySpec struct {
	Path    string `json:"path" validate:"required"`
	Mode    string `json:"mode,omitempty"`    // "rw" | "overlay" | "ro"
	Persist *bool  `json:"persist,omitempty"` // overlay mode only; default true
}

type EnvPassthroughSpec struct {
	Mode string   `json:"mode,omitempty"` // "deny" | "allow"
	Keys []string `json:"keys,omitempty"`
}

type BindMount struct {
	Source   string `json:"source" validate:"required"`
	Dest     string `json:"dest,omitempty"`
	ReadOnly bool   `json:"readonly,omitempty"`
}

type IsolatedCreateSessionResponse struct {
	SessionID string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
}

func (r *CreateIsolatedSessionRequest) Validate() error {
	v := validator.New()
	if err := v.Struct(r); err != nil {
		return err
	}
	if r.Workspace == nil && len(r.Overlays) == 0 {
		return fmt.Errorf("workspace or overlays is required")
	}
	// The effective mount list (legacy workspace prepended) must carry
	// absolute, unique paths: CreateIsolatedSession runs host-side
	// MkdirAll on each path before bwrap validates, so a relative or
	// duplicated path must fail here with 400 instead of creating stray
	// host directories and surfacing as a 500.
	seenPaths := make(map[string]struct{}, len(r.Overlays)+1)
	for _, ov := range r.EffectiveOverlays() {
		if !strings.HasPrefix(ov.Path, "/") {
			return fmt.Errorf("overlays: path %q must be an absolute path", ov.Path)
		}
		cleaned := filepath.Clean(ov.Path)
		if _, dup := seenPaths[cleaned]; dup {
			return fmt.Errorf("overlays: duplicate path %q", ov.Path)
		}
		seenPaths[cleaned] = struct{}{}
	}
	if r.Workspace != nil && r.Workspace.Mode != "" {
		switch r.Workspace.Mode {
		case WorkspaceModeRW, WorkspaceModeOverlay, WorkspaceModeRO:
		default:
			return fmt.Errorf("invalid workspace mode %q: must be %s, %s, or %s",
				r.Workspace.Mode, WorkspaceModeRW, WorkspaceModeOverlay, WorkspaceModeRO)
		}
	}
	for i, ov := range r.Overlays {
		if ov.Mode != "" {
			switch ov.Mode {
			case WorkspaceModeRW, WorkspaceModeOverlay, WorkspaceModeRO:
			default:
				return fmt.Errorf("invalid overlays[%d] mode %q: must be %s, %s, or %s",
					i, ov.Mode, WorkspaceModeRW, WorkspaceModeOverlay, WorkspaceModeRO)
			}
		}
		if ov.Persist != nil &&
			ov.Mode != WorkspaceModeOverlay && ov.Mode != "" {
			return fmt.Errorf("overlays[%d]: persist applies only to mode %q",
				i, WorkspaceModeOverlay)
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

// EffectiveOverlays returns the request's overlays with the legacy
// workspace field (when present) prepended as the primary overlay,
// mirroring the documented request semantics.
func (r *CreateIsolatedSessionRequest) EffectiveOverlays() []OverlaySpec {
	overlays := make([]OverlaySpec, 0, len(r.Overlays)+1)
	if r.Workspace != nil {
		overlays = append(overlays, OverlaySpec{Path: r.Workspace.Path, Mode: r.Workspace.Mode})
	}
	overlays = append(overlays, r.Overlays...)
	return overlays
}

// Run

type IsolatedRunRequest struct {
	Code           string            `json:"code" validate:"required"`
	Envs           map[string]string `json:"envs,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" validate:"omitempty,gte=0"`
	Background     bool              `json:"background,omitempty"`
}

func (r *IsolatedRunRequest) Validate() error {
	v := validator.New()
	return v.Struct(r)
}

type IsolatedBackgroundRunResponse struct {
	SessionID string    `json:"session_id"`
	RunID     string    `json:"run_id"`
	StartedAt time.Time `json:"started_at"`
}

type IsolatedRunStatus struct {
	SessionID  string     `json:"session_id"`
	RunID      string     `json:"run_id"`
	Running    bool       `json:"running"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Session State

// SessionState is returned by GET /v1/isolated/session/<id>.
//
// Runtime fields (Status/CreatedAt/LastRunAt/IdleRemainingSeconds) are always
// populated. The remaining fields echo the parameters used to create the
// session and let a stateless client rebuild a session handle from just a
// session ID (e.g. after a client restart). Older execd builds may omit
// these fields; clients must tolerate them being absent.
type SessionState struct {
	Status               string    `json:"status"` // "active" | "dead" | "destroyed"
	CreatedAt            time.Time `json:"created_at"`
	LastRunAt            time.Time `json:"last_run_at"`
	IdleRemainingSeconds *int      `json:"idle_remaining_seconds,omitempty"`

	// Creation-parameter echoes. All optional; a session_id-only client
	// must tolerate any of these being absent. Workspace is echoed only
	// for sessions with a single overlay (the legacy sugar shape); the
	// full effective list is always available in Overlays.
	Profile            string              `json:"profile,omitempty"`
	Workspace          *WorkspaceSpec      `json:"workspace,omitempty"`
	Overlays           []OverlaySpec       `json:"overlays,omitempty"`
	ExtraWritable      []string            `json:"extra_writable,omitempty"`
	Binds              []BindMount         `json:"binds,omitempty"`
	ShareNet           *bool               `json:"share_net,omitempty"`
	EnvPassthrough     *EnvPassthroughSpec `json:"env_passthrough,omitempty"`
	Uid                *uint32             `json:"uid,omitempty"`
	Gid                *uint32             `json:"gid,omitempty"`
	UidMode            string              `json:"uid_mode,omitempty"`
	IdleTimeoutSeconds *int                `json:"idle_timeout_seconds,omitempty"`
}

type IsolatedSessionSummary struct {
	SessionID            string    `json:"session_id"`
	Status               string    `json:"status"` // "active" | "dead"
	CreatedAt            time.Time `json:"created_at"`
	LastRunAt            time.Time `json:"last_run_at"`
	IdleRemainingSeconds *int      `json:"idle_remaining_seconds,omitempty"`
}

type ListIsolatedSessionsResponse struct {
	Sessions []IsolatedSessionSummary `json:"sessions"`
}

// Capabilities

type CapabilitiesResponse struct {
	Available        bool               `json:"available"`
	Isolator         string             `json:"isolator,omitempty"`
	Version          string             `json:"version,omitempty"`
	Message          string             `json:"message,omitempty"`
	SetprivAvailable bool               `json:"setpriv_available"`
	UsernsAvailable  bool               `json:"userns_available"`
	CommitSupported  bool               `json:"commit_supported"`
	DiffSupported    bool               `json:"diff_supported"`
	Hardening        *HardeningStatus   `json:"hardening,omitempty"`
	RuntimeInit      *RuntimeInitStatus `json:"runtimeInit,omitempty"`
}
