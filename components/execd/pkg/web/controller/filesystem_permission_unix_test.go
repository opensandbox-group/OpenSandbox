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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFilesystemMakeDirsAppliesExplicitMode(t *testing.T) {
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
			target := filepath.Join(t.TempDir(), "new", "directory")
			t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
			rec := postPermissionRequest(t, "/directories", map[string]any{
				target: map[string]any{"mode": tt.mode},
			})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, tt.want, info.Mode()&^os.ModeDir)
		})
	}
}

func TestFilesystemPermissionModesRetainNumericBeforeNull(t *testing.T) {
	for _, route := range []string{"/directories", "/files/permissions"} {
		t.Run(route, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "target")
			if route == "/files/permissions" {
				require.NoError(t, os.WriteFile(target, nil, 0o640))
			}
			key, err := json.Marshal(target)
			require.NoError(t, err)
			body := []byte(fmt.Sprintf(`{%s:{"mode":600,"mode":null}}`, key))
			ctrl, rec := newFilesystemController(t, http.MethodPost, route, body)
			if route == "/directories" {
				ctrl.MakeDirs()
			} else {
				ctrl.ChmodFiles()
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		})
	}
}

func TestFilesystemMakeDirsPreservesExistingMode(t *testing.T) {
	target := t.TempDir()
	require.NoError(t, os.Chmod(target, 0o750))
	rec := postPermissionRequest(t, "/directories", map[string]any{
		target: map[string]any{"mode": 0},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
}

func TestFilesystemChmodFilesAppliesExplicitZero(t *testing.T) {
	target := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(target, nil, 0o640))
	t.Cleanup(func() { _ = os.Chmod(target, 0o600) })
	rec := postPermissionRequest(t, "/files/permissions", map[string]any{
		target: map[string]any{"mode": 0},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Zero(t, info.Mode().Perm())
}

func TestFilesystemChmodFilesOwnerChangeKeepsSetuid(t *testing.T) {
	other := otherUploadAccount(t)
	requireSpecialBitSupport(t)
	target := filepath.Join(t.TempDir(), "helper")
	require.NoError(t, os.WriteFile(target, nil, 0o640))
	rec := postPermissionRequest(t, "/files/permissions", map[string]any{
		target: map[string]any{"mode": 4755, "owner": other.owner, "group": other.group},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireOwnedBy(t, other, target)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755)|os.ModeSetuid, info.Mode())
}

func TestFilesystemChmodFilesInvalidModeDoesNotChangeOwner(t *testing.T) {
	other := otherUploadAccount(t)
	current := currentUploadAccount(t)
	target := filepath.Join(t.TempDir(), "helper")
	require.NoError(t, os.WriteFile(target, nil, 0o640))
	rec := postPermissionRequest(t, "/files/permissions", map[string]any{
		target: map[string]any{"mode": 10000, "owner": other.owner, "group": other.group},
	})
	assertInvalidPermissionMode(t, rec)
	requireOwnedBy(t, current, target)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode())
}
