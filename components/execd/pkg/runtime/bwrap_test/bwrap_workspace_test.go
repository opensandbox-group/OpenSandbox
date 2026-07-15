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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestWorkspaceRW(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-rw"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "original.txt"), []byte("ok"), 0o644))

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo hello && echo world", nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Equal(t, []string{"hello", "world"}, lines)
}

func TestWorkspaceReadOnly(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-ro"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "readme.txt"), []byte("readonly-data"), 0o644))

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "ro",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "echo hello-ro", nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Equal(t, []string{"hello-ro"}, lines)
}

func TestWorkspaceOverlay(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-overlay"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "original.txt"), []byte("original"), 0o644))

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "overlay",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Read original + write new file in single command via overlay.
	newFile := filepath.Join(wsDir, "upper-only.txt")
	var lines []string
	err = r.RunInIsolatedSession(ctx, id,
		"echo overlay-read && echo upper-data > "+newFile, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Equal(t, []string{"overlay-read"}, lines)

	// Overlay writes go to upper, must NOT leak to host workspace.
	_, err = os.ReadFile(newFile)
	assert.True(t, os.IsNotExist(err), "overlay mode: file should NOT leak to host workspace")
}

func TestConcurrentSessions(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-conc"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "strict", WorkspacePath: wsDir, WorkspaceMode: "rw",
	}

	id1, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id1)

	id2, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var out1, out2 []string
	require.NoError(t, r.RunInIsolatedSession(ctx, id1, "echo one", nil, func(l string) { out1 = append(out1, l) }))
	require.NoError(t, r.RunInIsolatedSession(ctx, id2, "echo two", nil, func(l string) { out2 = append(out2, l) }))
	assert.Equal(t, "one", out1[0])
	assert.Equal(t, "two", out2[0])
}
