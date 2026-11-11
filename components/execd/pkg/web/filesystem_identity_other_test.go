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

//go:build !linux

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestFilesystemIdentityUnsupportedPlatformDoesNotMutateFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(path, []byte("unchanged"), 0600))
	router := gin.New()
	registerFilesystemRoutes(router.Group("/v1/filesystem/:uid/:gid", filesystemIdentityMiddleware()))
	request := httptest.NewRequest(http.MethodDelete, "/v1/filesystem/0/0/files?path="+url.QueryEscape(path), nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusNotImplemented, response.Code)
	require.Contains(t, response.Body.String(), "FILESYSTEM_IDENTITY_UNAVAILABLE")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "unchanged", string(content))
}
