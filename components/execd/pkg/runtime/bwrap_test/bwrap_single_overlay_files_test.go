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

//go:build linux && bwrap

package bwrap_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

// TestSingleOverlayFiles_PathOutsideWorkspaceRejected pins the default
// single-overlay session shape: absolute paths outside the workspace (the
// shared /tmp, /etc, ...) are rejected with ErrPathOutsideOverlays instead
// of silently resolving against the workspace upper, where they would touch
// a different file than the namespace path refers to. session.run keeps
// full namespace access; the files API serves exactly the session mounts.
func TestSingleOverlayFiles_PathOutsideWorkspaceRejected(t *testing.T) {
	r := newRunner(t)

	wsDir := "/tmp/bwrap-test-single-overlay-files"
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	defer os.RemoveAll(wsDir)

	opts := &runtime.IsolatedSessionOptions{
		Profile: "balanced", WorkspacePath: wsDir, WorkspaceMode: "overlay",
	}
	id, err := r.CreateIsolatedSession(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.DeleteIsolatedSession(id) })

	// The run direction works and can write both the workspace and /tmp.
	escaped := "/tmp/single-overlay-escape.txt"
	runInSession(t, r, id,
		"echo run-data > "+filepath.Join(wsDir, "run.txt")+" && echo ns-data > "+escaped, nil)

	// Out-of-scope absolute paths are rejected, not silently joined into
	// the workspace upper.
	mv, err := r.GetMergedView(id)
	require.NoError(t, err)
	for _, p := range []string{escaped, "/etc/passwd"} {
		_, err := mv.ReadFile(p)
		require.Error(t, err)
		assert.True(t, errors.Is(err, isolation.ErrPathOutsideOverlays), "ReadFile %s = %v", p, err)
	}
	require.Error(t, mv.WriteFile(escaped, []byte("x"), 0o644))

	// The rejected write never landed in the workspace upper.
	_, err = os.Stat(filepath.Join(wsDir, "tmp", filepath.Base(escaped)))
	assert.True(t, os.IsNotExist(err), "rejected write leaked into the workspace upper")

	// Run→API direction on the workspace itself still works.
	data, err := mv.ReadFile(filepath.Join(wsDir, "run.txt"))
	require.NoError(t, err)
	assert.Equal(t, "run-data\n", string(data))
}
