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

//go:build !windows

package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/vfs"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

// permissionView exercises the controller helpers without an isolation runtime.
// MkdirAll assigns ownership like the session views do; other methods are unused.
type permissionView struct {
	vfs.FS
	root      string
	mutations []string
}

func (v *permissionView) Stat(path string) (os.FileInfo, error) {
	return os.Stat(filepath.Join(v.root, path))
}

func (v *permissionView) MkdirAll(path string, mode os.FileMode) error {
	v.mutations = append(v.mutations, "mkdir/chown")
	target := filepath.Join(v.root, path)
	if err := os.MkdirAll(target, mode); err != nil {
		return err
	}
	return os.Chown(target, os.Getuid(), os.Getgid())
}

func (v *permissionView) Chmod(path string, mode os.FileMode) error {
	v.mutations = append(v.mutations, "chmod")
	return os.Chmod(filepath.Join(v.root, path), mode)
}

func permissionFromJSON(t *testing.T, data string) model.Permission {
	t.Helper()
	var perm model.Permission
	require.NoError(t, json.Unmarshal([]byte(data), &perm))
	return perm
}

func TestIsolatedPermissionHelpersApplyExplicitModes(t *testing.T) {
	for _, tt := range []struct {
		mode int
		want os.FileMode
	}{
		{2775, 0o775 | os.ModeSetgid},
		{1777, 0o777 | os.ModeSticky},
		{0, 0},
	} {
		t.Run(fmt.Sprint(tt.mode), func(t *testing.T) {
			if tt.mode != 0 {
				requireSpecialBitSupport(t)
			}
			view := &permissionView{root: t.TempDir()}
			perm := permissionFromJSON(t, fmt.Sprintf(`{"mode":%d,"owner":"ignored","group":"ignored"}`, tt.mode))
			target := filepath.Join(view.root, "parent", "directory")
			t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
			require.NoError(t, makeIsolatedDir(view, "parent/directory", perm))
			info, err := view.Stat("parent/directory")
			require.NoError(t, err)
			require.Equal(t, tt.want, info.Mode()&^os.ModeDir)
			require.Equal(t, []string{"mkdir/chown", "chmod"}, view.mutations)

			file := filepath.Join(view.root, "file")
			require.NoError(t, os.WriteFile(file, nil, 0o640))
			require.NoError(t, chmodIsolatedFile(view, "file", perm))
			info, err = view.Stat("file")
			require.NoError(t, err)
			require.Equal(t, tt.want, info.Mode())
		})
	}
}

func TestIsolatedPermissionHelpersRejectInvalidBeforeChanges(t *testing.T) {
	for _, mode := range []int{-1, 8, 7778, 10000, 100644} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			view := &permissionView{root: t.TempDir()}
			perm := permissionFromJSON(t, fmt.Sprintf(`{"mode":%d}`, mode))
			require.Error(t, makeIsolatedDir(view, "parent/directory", perm))
			require.Error(t, chmodIsolatedFile(view, "file", perm))
			require.Empty(t, view.mutations)
			entries, err := os.ReadDir(view.root)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestIsolatedPermissionHelpersKeepOmittedDefaults(t *testing.T) {
	for _, data := range []string{`{}`, `{"mode":null}`} {
		t.Run(data, func(t *testing.T) {
			view := &permissionView{root: t.TempDir()}
			perm := permissionFromJSON(t, data)
			reference := filepath.Join(view.root, "reference")
			require.NoError(t, os.Mkdir(reference, 0o755))
			before, err := os.Stat(reference)
			require.NoError(t, err)
			require.NoError(t, makeIsolatedDir(view, "new", perm))
			after, err := view.Stat("new")
			require.NoError(t, err)
			require.Equal(t, before.Mode(), after.Mode())
			require.NoError(t, chmodIsolatedFile(view, "new", perm))
			require.Equal(t, []string{"mkdir/chown"}, view.mutations)
		})
	}
}

func TestIsolatedMakeDirPreservesExistingMode(t *testing.T) {
	view := &permissionView{root: t.TempDir()}
	require.NoError(t, os.Mkdir(filepath.Join(view.root, "existing"), 0o750))
	require.NoError(t, os.Chmod(filepath.Join(view.root, "existing"), 0o750))
	perm := permissionFromJSON(t, `{"mode":0}`)
	require.NoError(t, makeIsolatedDir(view, "existing", perm))
	info, err := view.Stat("existing")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
	require.Equal(t, []string{"mkdir/chown"}, view.mutations)
}
