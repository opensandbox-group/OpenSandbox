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

// Package isolation provides per-execution namespace isolation via bubblewrap.
package isolation

import "os/exec"

// Profile presets default isolation settings.
type Profile string

const (
	ProfileStrict   Profile = "strict"
	ProfileBalanced Profile = "balanced"
)

// Valid reports whether p is a known profile name.
func (p Profile) Valid() bool {
	return p == ProfileStrict || p == ProfileBalanced
}

// WorkspaceMode controls how the workspace directory is mounted into the
// isolated namespace.
type WorkspaceMode string

const (
	WorkspaceRW      WorkspaceMode = "rw"
	WorkspaceOverlay WorkspaceMode = "overlay"
	WorkspaceRO      WorkspaceMode = "ro"
)

// Valid reports whether m is a known workspace mode.
func (m WorkspaceMode) Valid() bool {
	return m == WorkspaceRW || m == WorkspaceOverlay || m == WorkspaceRO
}

// EnvMode controls how host environment variables are passed through to the
// isolated namespace.
type EnvMode string

const (
	EnvModeDeny  EnvMode = "deny"
	EnvModeAllow EnvMode = "allow"
)

// Valid reports whether m is a known env passthrough mode.
func (m EnvMode) Valid() bool {
	return m == EnvModeDeny || m == EnvModeAllow
}

// UidMode controls how user identity is established inside the namespace.
type UidMode string

const (
	// UidModeSetpriv uses setpriv(1) after bwrap to drop privileges via
	// real setuid/setgid. Requires CAP_SETUID/CAP_SETGID or root.
	// This is the default when UidMode is empty.
	UidModeSetpriv UidMode = "setpriv"

	// UidModeUserns creates a user namespace (--unshare-user) and maps the
	// desired uid/gid inside it via --uid/--gid. Also passes
	// --disable-userns to prevent nested user namespace creation.
	// Does not require elevated privileges.
	UidModeUserns UidMode = "userns"
)

// Valid reports whether m is a known uid mode.
func (m UidMode) Valid() bool {
	return m == UidModeSetpriv || m == UidModeUserns
}

// Structs

// WorkspaceSpec describes a workspace directory and how it is mounted.
type WorkspaceSpec struct {
	Path string
	Mode WorkspaceMode
}

// EnvSpec controls environment variable passthrough into the namespace.
type EnvSpec struct {
	Mode EnvMode
	Keys []string // allowlist (mode=allow) or denylist (mode=deny)
}

// BindMount describes an additional host path bind-mounted into the namespace
// with an explicit source-to-destination mapping. Unlike WrapOptions.ExtraWritable
// (which always mounts Source==Dest read-write), a BindMount may map a distinct
// destination and be mounted read-only.
type BindMount struct {
	Source   string // host path (required)
	Dest     string // mount destination; defaults to Source when empty
	ReadOnly bool   // true → --ro-bind; false → --bind
}

// Capabilities describes what the isolator can and cannot do.
type Capabilities struct {
	Available              bool
	Isolator               string
	Version                string
	Profiles               []Profile
	AllowedWorkspaces      []string
	AllowedExtraWritable   []string
	ShareNetOverridable    bool
	CommitSupported        bool
	DiffSupported          bool
	SeccompProfileSHA256   string
	PersistAvailable       bool
	PersistMaxBytesDefault int64
	PersistMaxBytesLimit   int64
	PersistRetainDefault   int64 // seconds
}

// WrapOptions configures a single isolated execution.
type WrapOptions struct {
	Profile        Profile
	Workspace      WorkspaceSpec
	ExtraWritable  []string
	Binds          []BindMount
	ShareNet       bool
	EnvPassthrough EnvSpec
	Uid, Gid       *uint32
	UidMode        UidMode // "" or "setpriv" → setpriv; "userns" → user namespace
	UpperDir       string  // empty when upper is on tmpfs (persist disabled)
	WorkDir        string
}

// Interface

// Isolator wraps an *exec.Cmd in a namespace-isolated execution environment.
type Isolator interface {
	Name() string
	Available() bool
	Capabilities() Capabilities
	Wrap(cmd *exec.Cmd, opts WrapOptions) error
}
