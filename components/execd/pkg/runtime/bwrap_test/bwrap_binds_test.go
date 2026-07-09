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

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

// TestBinds_SourceToDest verifies a host path can be bind-mounted at a distinct
// destination inside the namespace and written through.
func TestBinds_SourceToDest(t *testing.T) {
	r := newRunner(t)

	srcDir := t.TempDir()
	// Destination must be an existing mount point inside the namespace. Use a
	// separate temp dir under /tmp (bind-mounted into the namespace) so bwrap
	// can bind onto it without needing to create a dir under a read-only mount.
	destDir := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		Binds: []isolation.BindMount{
			{Source: srcDir, Dest: destDir},
		},
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Write via the in-namespace destination path.
	err = r.RunInIsolatedSession(ctx, id, "echo 'mapped-data' > "+destDir+"/out.txt", nil, nil)
	require.NoError(t, err, "writing to mapped bind dest should succeed")

	// Verify it landed on the host source dir.
	data, err := os.ReadFile(filepath.Join(srcDir, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "mapped-data\n", string(data))
}

// TestBinds_ReadOnly verifies a read-only bind rejects writes but allows reads.
func TestBinds_ReadOnly(t *testing.T) {
	r := newRunner(t)

	srcDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "ro.txt"), []byte("readonly-value\n"), 0o644))
	// Destination is a separate existing mount point under /tmp.
	destDir := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		Binds: []isolation.BindMount{
			{Source: srcDir, Dest: destDir, ReadOnly: true},
		},
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Read should succeed.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "cat "+destDir+"/ro.txt", nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Equal(t, []string{"readonly-value"}, lines)

	// Write should fail (non-zero exit).
	err = r.RunInIsolatedSession(ctx, id, "echo x > "+destDir+"/new.txt", nil, nil)
	require.Error(t, err, "writing to a read-only bind should fail")
}

// TestBinds_SourceNotInAllowlist verifies binds are rejected when the source
// path is outside the writable allowlist.
func TestBinds_SourceNotInAllowlist(t *testing.T) {
	r := newRunnerWithConfig(t, isolation.Config{
		UpperRoot:       t.TempDir(),
		UpperMaxBytes:   1 << 30,
		AllowedWritable: []string{"/tmp/allowed-only"},
	})

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		Binds: []isolation.BindMount{
			{Source: "/etc", Dest: "/mnt/etc"},
		},
	}

	_, err := r.CreateIsolatedSession(opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in allowlist")
}
