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

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postPermissionRequest(t *testing.T, route string, request map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	require.NoError(t, err)
	ctrl, rec := newFilesystemController(t, http.MethodPost, route, body)
	if route == "/directories" {
		ctrl.MakeDirs()
	} else {
		ctrl.ChmodFiles()
	}
	return rec
}

func assertInvalidPermissionMode(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, model.ErrorCodeInvalidFileMetadata, resp.Code)
}

func TestFilesystemPermissionModesRejectInvalidBeforeChanges(t *testing.T) {
	for _, mode := range []int{-1, 8, 9, 7778, 10000, 100644} {
		t.Run(fmt.Sprintf("mode_%d", mode), func(t *testing.T) {
			t.Run("directories", func(t *testing.T) {
				parent := filepath.Join(t.TempDir(), "missing")
				target := filepath.Join(parent, "nested")
				rec := postPermissionRequest(t, "/directories", map[string]any{
					target: map[string]any{"mode": mode},
				})
				assertInvalidPermissionMode(t, rec)
				_, err := os.Stat(parent)
				require.ErrorIs(t, err, os.ErrNotExist, "invalid mode must not create parents")
			})
			t.Run("permissions", func(t *testing.T) {
				target := filepath.Join(t.TempDir(), "existing")
				require.NoError(t, os.WriteFile(target, []byte("keep"), 0o640))
				before, err := os.Stat(target)
				require.NoError(t, err)
				rec := postPermissionRequest(t, "/files/permissions", map[string]any{
					target: map[string]any{"mode": mode},
				})
				assertInvalidPermissionMode(t, rec)
				after, err := os.Stat(target)
				require.NoError(t, err)
				assert.Equal(t, before.Mode(), after.Mode())
				assert.Equal(t, before.ModTime(), after.ModTime())
				assert.True(t, os.SameFile(before, after))
			})
		})
	}
}

func TestFilesystemPermissionModesValidateWholeRequest(t *testing.T) {
	for _, route := range []string{"/directories", "/files/permissions"} {
		t.Run(route, func(t *testing.T) {
			dir := t.TempDir()
			valid := filepath.Join(dir, "valid")
			invalid := filepath.Join(dir, "invalid")
			if route == "/files/permissions" {
				require.NoError(t, os.WriteFile(valid, nil, 0o644))
				require.NoError(t, os.WriteFile(invalid, nil, 0o644))
			}
			rec := postPermissionRequest(t, route, map[string]any{
				valid:   map[string]any{"mode": 0},
				invalid: map[string]any{"mode": 10000},
			})
			assertInvalidPermissionMode(t, rec)
			if route == "/directories" {
				entries, err := os.ReadDir(dir)
				require.NoError(t, err)
				require.Empty(t, entries, "no request entry may be created before validation")
			} else {
				for _, path := range []string{valid, invalid} {
					info, err := os.Stat(path)
					require.NoError(t, err)
					require.NotZero(t, info.Mode().Perm()&0o200, "validation must precede chmod")
				}
			}
		})
	}
}

func TestFilesystemPermissionModesOmittedKeepDefaults(t *testing.T) {
	for _, permission := range []map[string]any{{}, {"mode": nil}} {
		t.Run(fmt.Sprint(permission), func(t *testing.T) {
			dir := t.TempDir()
			reference := filepath.Join(dir, "reference")
			require.NoError(t, os.Mkdir(reference, os.ModePerm))
			before, err := os.Stat(reference)
			require.NoError(t, err)
			target := filepath.Join(dir, "new")
			rec := postPermissionRequest(t, "/directories", map[string]any{target: permission})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			after, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, before.Mode(), after.Mode())

			file := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(file, nil, 0o640))
			before, err = os.Stat(file)
			require.NoError(t, err)
			rec = postPermissionRequest(t, "/files/permissions", map[string]any{file: permission})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			after, err = os.Stat(file)
			require.NoError(t, err)
			require.Equal(t, before.Mode(), after.Mode())
		})
	}
}
