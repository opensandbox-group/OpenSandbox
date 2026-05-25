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

package opensandbox

import (
	"context"
	"time"
)

// SandboxManager provides administrative operations on sandboxes
// without connecting to a specific sandbox.
type SandboxManager struct {
	lifecycle *LifecycleClient
}

// NewSandboxManager creates a SandboxManager from the given connection config.
func NewSandboxManager(config ConnectionConfig) *SandboxManager {
	return &SandboxManager{
		lifecycle: config.lifecycleClient(),
	}
}

// ListSandboxInfos returns a paginated list of sandboxes with optional filtering.
func (m *SandboxManager) ListSandboxInfos(ctx context.Context, filter ListOptions) (*ListSandboxesResponse, error) {
	return m.lifecycle.ListSandboxes(ctx, filter)
}

// GetSandboxInfo retrieves info for a single sandbox by ID.
func (m *SandboxManager) GetSandboxInfo(ctx context.Context, sandboxID string) (*SandboxInfo, error) {
	return m.lifecycle.GetSandbox(ctx, sandboxID)
}

// PatchSandboxMetadata patches metadata for a sandbox by ID.
func (m *SandboxManager) PatchSandboxMetadata(ctx context.Context, sandboxID string, patch MetadataPatch) (*SandboxInfo, error) {
	return m.lifecycle.PatchSandboxMetadata(ctx, sandboxID, patch)
}

// KillSandbox terminates a sandbox by ID.
func (m *SandboxManager) KillSandbox(ctx context.Context, sandboxID string) error {
	return m.lifecycle.DeleteSandbox(ctx, sandboxID)
}

// PauseSandbox pauses a running sandbox by ID.
func (m *SandboxManager) PauseSandbox(ctx context.Context, sandboxID string) error {
	return m.lifecycle.PauseSandbox(ctx, sandboxID)
}

// ResumeSandbox resumes a paused sandbox by ID.
func (m *SandboxManager) ResumeSandbox(ctx context.Context, sandboxID string) error {
	return m.lifecycle.ResumeSandbox(ctx, sandboxID)
}

// RenewSandbox extends a sandbox's expiration by the given duration from now.
func (m *SandboxManager) RenewSandbox(ctx context.Context, sandboxID string, duration time.Duration) (*RenewExpirationResponse, error) {
	return m.lifecycle.RenewExpiration(ctx, sandboxID, time.Now().Add(duration))
}

// CreateSnapshot creates a snapshot for the given sandbox.
func (m *SandboxManager) CreateSnapshot(ctx context.Context, sandboxID string, req CreateSnapshotRequest) (*SnapshotInfo, error) {
	return m.lifecycle.CreateSnapshot(ctx, sandboxID, req)
}

// GetSnapshot retrieves snapshot info by ID.
func (m *SandboxManager) GetSnapshot(ctx context.Context, snapshotID string) (*SnapshotInfo, error) {
	return m.lifecycle.GetSnapshot(ctx, snapshotID)
}

// ListSnapshots returns a paginated list of snapshots with optional filtering.
func (m *SandboxManager) ListSnapshots(ctx context.Context, filter ListSnapshotsOptions) (*ListSnapshotsResponse, error) {
	return m.lifecycle.ListSnapshots(ctx, filter)
}

// DeleteSnapshot deletes a snapshot by ID.
func (m *SandboxManager) DeleteSnapshot(ctx context.Context, snapshotID string) error {
	return m.lifecycle.DeleteSnapshot(ctx, snapshotID)
}

// Close releases local resources. Currently a no-op placeholder.
func (m *SandboxManager) Close() error { return nil }
