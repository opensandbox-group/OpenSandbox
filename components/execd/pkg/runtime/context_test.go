// Copyright 2025 Alibaba Group Holding Ltd.
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

package runtime

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListContextsAndNewIpynbPath(t *testing.T) {
	c := NewController("http://example", "token")
	c.jupyterClientMap.Store("session-python", &jupyterKernel{language: Python})
	c.defaultLanguageSessions.Store(Go, "session-go-default")

	pyContexts, err := c.listLanguageContexts(Python)
	require.NoError(t, err)
	require.Len(t, pyContexts, 1)
	require.Equal(t, "session-python", pyContexts[0].ID)
	require.Equal(t, Python, pyContexts[0].Language)

	allContexts, err := c.listAllContexts()
	require.NoError(t, err)
	require.Len(t, allContexts, 2)

	tmpDir := filepath.Join(t.TempDir(), "nested")
	path, err := c.newIpynbPath("abc123", tmpDir)
	require.NoError(t, err)
	_, statErr := os.Stat(tmpDir)
	require.NoError(t, statErr, "expected directory to be created")
	expected := filepath.Join(tmpDir, "abc123.ipynb")
	require.Equal(t, expected, path)
}

func TestNewContextID_UniqueAndLength(t *testing.T) {
	c := NewController("", "")
	id1 := c.newContextID()
	id2 := c.newContextID()

	require.NotEmpty(t, id1)
	require.NotEmpty(t, id2)
	require.NotEqual(t, id1, id2, "expected unique ids")
	require.Len(t, id1, 32)
	require.Len(t, id2, 32)
}

func TestNewIpynbPath_ErrorWhenCwdIsFile(t *testing.T) {
	c := NewController("", "")
	tmpFile := filepath.Join(t.TempDir(), "file.txt")
	require.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0o644))

	_, err := c.newIpynbPath("abc", tmpFile)
	require.Error(t, err, "expected error when cwd is a file")
}

func TestNewIpynbPath_ExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	c := NewController("", "")
	path, err := c.newIpynbPath("abc", "~/workspace")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "workspace", "abc.ipynb"), path)
}

func TestListContextUnsupportedLanguage(t *testing.T) {
	c := NewController("", "")
	_, err := c.ListContext(Command.String())
	require.Error(t, err, "expected error for command language")
	_, err = c.ListContext(BackgroundCommand.String())
	require.Error(t, err, "expected error for background-command language")
	_, err = c.ListContext(SQL.String())
	require.Error(t, err, "expected error for sql language")
}

func TestDeleteContext_NotFound(t *testing.T) {
	c := NewController("", "")
	err := c.DeleteContext("missing")
	require.Error(t, err, "expected ErrContextNotFound")
	require.ErrorIs(t, err, ErrContextNotFound)
}

func TestGetContext_NotFound(t *testing.T) {
	c := NewController("", "")

	_, err := c.GetContext("missing")
	require.Error(t, err, "expected ErrContextNotFound")
	require.ErrorIs(t, err, ErrContextNotFound)
}

func TestDeleteContext_RemovesCacheOnSuccess(t *testing.T) {
	sessionID := "sess-123"

	// mock jupyter server that accepts DELETE
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method, "unexpected method")
		require.True(t, strings.HasSuffix(r.URL.Path, "/api/sessions/"+sessionID), "unexpected path: %s", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := NewController(server.URL, "token")
	c.jupyterClientMap.Store(sessionID, &jupyterKernel{language: Python})
	c.defaultLanguageSessions.Store(Python, sessionID)

	require.NoError(t, c.DeleteContext(sessionID))

	require.Nil(t, c.getJupyterKernel(sessionID), "expected cache to be cleared")
	_, ok := c.defaultLanguageSessions.Load(Python)
	require.False(t, ok, "expected default session entry to be removed")
}

func TestDeleteLanguageContext_RemovesCacheOnSuccess(t *testing.T) {
	lang := Python
	session1 := "sess-1"
	session2 := "sess-2"

	// mock jupyter server to accept two deletes
	deleteCalls := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method, "unexpected method")
		if strings.Contains(r.URL.Path, session1) {
			deleteCalls[session1]++
		} else if strings.Contains(r.URL.Path, session2) {
			deleteCalls[session2]++
		} else {
			require.Failf(t, "unexpected path", "%s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	c := NewController(server.URL, "token")
	c.jupyterClientMap.Store(session1, &jupyterKernel{language: lang})
	c.jupyterClientMap.Store(session2, &jupyterKernel{language: lang})
	c.defaultLanguageSessions.Store(lang, session2)

	require.NoError(t, c.DeleteLanguageContext(lang))

	_, ok := c.jupyterClientMap.Load(session1)
	require.False(t, ok, "expected session1 removed from cache")
	_, ok = c.jupyterClientMap.Load(session2)
	require.False(t, ok, "expected session2 removed from cache")
	_, ok = c.defaultLanguageSessions.Load(lang)
	require.False(t, ok, "expected default entry removed")
	require.Equal(t, 1, deleteCalls[session1])
	require.Equal(t, 1, deleteCalls[session2])
}
