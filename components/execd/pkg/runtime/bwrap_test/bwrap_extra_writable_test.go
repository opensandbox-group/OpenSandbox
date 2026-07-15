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

// TestExtraWritable_WriteFile verifies that an ExtraWritable path can be
// written to via stdin pipe file redirection.
func TestExtraWritable_WriteFile(t *testing.T) {
	r := newRunner(t)

	extraDir := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		ExtraWritable: []string{extraDir},
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	testFile := filepath.Join(extraDir, "written-from-sandbox.txt")
	code := "echo 'extra-writable-data' > " + testFile
	err = r.RunInIsolatedSession(ctx, id, code, nil, nil)
	require.NoError(t, err, "writing to ExtraWritable path should succeed")

	// Verify the file exists on the host (since ExtraWritable is bind-mounted).
	data, err := os.ReadFile(testFile)
	require.NoError(t, err)
	assert.Equal(t, "extra-writable-data\n", string(data))
}

// TestExtraWritable_ReadWriteRoundTrip writes then reads back a file on an
// ExtraWritable path via the stdin pipe.
func TestExtraWritable_ReadWriteRoundTrip(t *testing.T) {
	r := newRunner(t)

	extraDir := t.TempDir()

	opts := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		ExtraWritable: []string{extraDir},
	}

	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	testFile := filepath.Join(extraDir, "roundtrip.txt")

	// Write.
	err = r.RunInIsolatedSession(ctx, id, "echo 'roundtrip-value' > "+testFile, nil, nil)
	require.NoError(t, err)

	// Read back.
	var lines []string
	err = r.RunInIsolatedSession(ctx, id, "cat "+testFile, nil,
		func(line string) { lines = append(lines, line) })
	require.NoError(t, err)
	assert.Equal(t, []string{"roundtrip-value"}, lines)
}

// TestExtraWritable_MultipleWrites writes to multiple ExtraWritable paths and
// verifies isolation between sessions.
func TestExtraWritable_MultipleWrites(t *testing.T) {
	r := newRunner(t)

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	opts1 := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		ExtraWritable: []string{dir1},
	}
	opts2 := &runtime.IsolatedSessionOptions{
		Profile:       "balanced",
		WorkspacePath: t.TempDir(),
		WorkspaceMode: "rw",
		ExtraWritable: []string{dir2},
	}

	id1, err := r.CreateIsolatedSession(opts1)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id1)

	id2, err := r.CreateIsolatedSession(opts2)
	require.NoError(t, err)
	defer r.DeleteIsolatedSession(id2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Session 1 writes to dir1, must not appear in dir2.
	require.NoError(t, r.RunInIsolatedSession(ctx, id1,
		"echo 's1-data' > "+filepath.Join(dir1, "s1.txt"), nil, nil))

	// Session 2 writes to dir2, must not appear in dir1.
	require.NoError(t, r.RunInIsolatedSession(ctx, id2,
		"echo 's2-data' > "+filepath.Join(dir2, "s2.txt"), nil, nil))

	// Verify host visibility.
	data, err := os.ReadFile(filepath.Join(dir1, "s1.txt"))
	require.NoError(t, err)
	assert.Equal(t, "s1-data\n", string(data))

	data, err = os.ReadFile(filepath.Join(dir2, "s2.txt"))
	require.NoError(t, err)
	assert.Equal(t, "s2-data\n", string(data))
}
