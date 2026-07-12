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

//go:build windows

package runtime

import (
	"context"
	"io"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/vfs"
)

// IsolatedSessionOptions bundles creation parameters (Windows stub).
type IsolatedSessionOptions struct {
	Profile            string
	WorkspacePath      string
	WorkspaceMode      string
	ExtraWritable      []string
	Binds              []isolation.BindMount
	ShareNet           *bool
	EnvPassthroughMode string
	EnvPassthroughKeys []string
	Uid                *uint32
	Gid                *uint32
	UidMode            string
	IdleTimeoutSeconds int
}

// StdoutCallback is called per line of stdout (Windows stub).
type StdoutCallback func(line string)

// IsolatedRunner is the isolated session runner (Windows stub).
type IsolatedRunner struct{}

// NewIsolatedRunner returns nil on Windows (isolation not supported).
func NewIsolatedRunner(_ *Controller, _ isolation.Isolator, _ isolation.Config) (*IsolatedRunner, error) {
	return &IsolatedRunner{}, nil
}

// StopGC is a no-op on Windows.
func (r *IsolatedRunner) StopGC() {}

// Available reports false on Windows.
func (r *IsolatedRunner) Available() bool { return false }

// CreateIsolatedSession returns an error on Windows.
func (r *IsolatedRunner) CreateIsolatedSession(_ *IsolatedSessionOptions) (string, error) {
	return "", ErrContextNotFound
}

// GetIsolatedSession returns an error on Windows.
func (r *IsolatedRunner) GetIsolatedSession(_ string) (*IsolatedSessionState, error) {
	return nil, ErrContextNotFound
}

// ListIsolatedSessions returns an empty list on Windows.
func (r *IsolatedRunner) ListIsolatedSessions() []IsolatedSessionSummary {
	return []IsolatedSessionSummary{}
}

// RunInIsolatedSession returns an error on Windows.
func (r *IsolatedRunner) RunInIsolatedSession(_ context.Context, _ string, _ string, _ map[string]string, _ StdoutCallback) error {
	return ErrContextNotFound
}

// DeleteIsolatedSession returns an error on Windows.
func (r *IsolatedRunner) DeleteIsolatedSession(_ string) error {
	return ErrContextNotFound
}

// DiffUpper returns an error on Windows.
func (r *IsolatedRunner) DiffUpper(_ string, _ io.Writer) error {
	return nil
}

// CommitUpper returns an error on Windows.
func (r *IsolatedRunner) CommitUpper(_ string) error {
	return nil
}

// GetMergedView returns an error on Windows.
func (r *IsolatedRunner) GetMergedView(_ string) (vfs.FS, error) {
	return nil, ErrContextNotFound
}

// Capabilities returns empty capabilities on Windows.
func (r *IsolatedRunner) Capabilities() isolation.Capabilities {
	return isolation.Capabilities{Available: false}
}

// IsolatedSessionState is the session state (Windows stub).
type IsolatedSessionState struct {
	Status               string
	CreatedAt            time.Time
	LastRunAt            time.Time
	IdleRemainingSeconds *int
}

// IsolatedSessionSummary describes a session in a list response (Windows stub).
type IsolatedSessionSummary struct {
	SessionID string
	IsolatedSessionState
}
