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

//go:build linux && bwrap

package bwrap_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestCreateSession_EmptyWorkspace(t *testing.T) {
	r := newRunner(t)
	_, err := r.CreateIsolatedSession(&runtime.IsolatedSessionOptions{})
	assert.Error(t, err, "should reject empty workspace path")
}

func TestGetSession_NotFound(t *testing.T) {
	r := newRunner(t)
	_, err := r.GetIsolatedSession("nonexistent-session-id")
	assert.Error(t, err)
}

func TestDeleteSession_NotFound(t *testing.T) {
	r := newRunner(t)
	err := r.DeleteIsolatedSession("nonexistent-session-id")
	assert.Error(t, err)
}

func TestRun_SessionNotFound(t *testing.T) {
	r := newRunner(t)
	err := r.RunInIsolatedSession(context.Background(), "nonexistent", "true", nil, nil)
	assert.Error(t, err)
}

func TestGetSession_AfterDelete(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	require.NoError(t, r.DeleteIsolatedSession(id))

	_, err = r.GetIsolatedSession(id)
	assert.Error(t, err, "session should not exist after delete")
}

func TestRun_AfterDelete(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	require.NoError(t, r.DeleteIsolatedSession(id))

	err = r.RunInIsolatedSession(context.Background(), id, "true", nil, nil)
	assert.Error(t, err, "should not be able to run on deleted session")
}

func TestRun_Timeout(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	// Note: context timeout propagation to bwrap process is not yet
	// implemented. Verify that the command completes and returns.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err = r.RunInIsolatedSession(ctx, id, "sleep 1", nil, nil)
	require.NoError(t, err, "sleep 1 should complete within timeout")
}

func TestDoubleDelete(t *testing.T) {
	r := newRunner(t)
	opts := &runtime.IsolatedSessionOptions{WorkspacePath: t.TempDir(), WorkspaceMode: "rw"}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	require.NoError(t, r.DeleteIsolatedSession(id))
	assert.Error(t, r.DeleteIsolatedSession(id), "second delete should fail")
}
