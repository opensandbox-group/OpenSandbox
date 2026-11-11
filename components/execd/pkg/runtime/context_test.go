// Copyright 2025 The OpenSandbox Authors
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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func writeJupyterServerFile(t *testing.T, rootDir, dirName, fileName string, info jupyterRuntimeServerInfo) string {
	t.Helper()
	runtimeDir := filepath.Join(rootDir, dirName)
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	serverFile := filepath.Join(runtimeDir, fileName)
	data, err := json.Marshal(info)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(serverFile, data, 0o600))
	return serverFile
}

func TestCreateAndDeleteContext_DiscoversStandaloneJupyterRuntimeToken(t *testing.T) {
	const discoveredToken = "standalone-random-token-64hex"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Token "+discoveredToken, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/kernelspecs"):
			_, _ = w.Write([]byte(`{"default":"python","kernelspecs":{"python":{"name":"python","spec":{"language":"python","display_name":"Python"}}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/sessions"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sess-discovered","kernel":{"id":"kernel-1","name":"python"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/kernels"):
			_, _ = w.Write([]byte(`[{"id":"kernel-1","name":"python"}]`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/sessions/sess-discovered"):
			w.WriteHeader(http.StatusNoContent)
		default:
			require.Failf(t, "unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	runtimeRoot := t.TempDir()
	port := jupyterPortFromBaseURL(server.URL)
	require.Greater(t, port, 0)
	writeJupyterServerFile(t, runtimeRoot, "opensandbox-jupyter.abc123", "jpserver-1.json", jupyterRuntimeServerInfo{
		Port:  port,
		Token: discoveredToken,
	})

	c := NewController(server.URL, "")
	c.jupyterRuntimeRoot = runtimeRoot

	sessionID, err := c.CreateContext(context.Background(), &CreateContextRequest{
		Language: Python,
		Cwd:      t.TempDir(),
	})
	require.NoError(t, err)
	require.Equal(t, "sess-discovered", sessionID)
	require.NoError(t, c.DeleteContext(sessionID))
}

func TestJupyterTokenDiscovery_FiltersAndPrecedence(t *testing.T) {
	const (
		port      = 44771
		baseURL   = "http://127.0.0.1:44771"
		oldToken  = "older-valid-token"
		newToken  = "newer-valid-token"
		initToken = "explicit-bootstrap-token"
	)

	runtimeRoot := t.TempDir()

	// 1. Mismatched port is ignored.
	writeJupyterServerFile(t, runtimeRoot, "opensandbox-jupyter.wrongport", "jpserver-1.json", jupyterRuntimeServerInfo{
		Port:  44772,
		Token: "wrong-port-token",
	})

	c := NewController(baseURL, "")
	c.jupyterRuntimeRoot = runtimeRoot
	require.Empty(t, c.jupyterToken())

	// 2. Newest valid server file wins.
	oldFile := writeJupyterServerFile(t, runtimeRoot, "opensandbox-jupyter.old", "jpserver-2.json", jupyterRuntimeServerInfo{
		Port:  port,
		Token: oldToken,
	})
	oldTime := time.Now().Add(-10 * time.Second)
	require.NoError(t, os.Chtimes(oldFile, oldTime, oldTime))

	newFile := writeJupyterServerFile(t, runtimeRoot, "opensandbox-jupyter.new", "jpserver-3.json", jupyterRuntimeServerInfo{
		Port:  port,
		Token: newToken,
	})
	newTime := time.Now().Add(-2 * time.Second)
	require.NoError(t, os.Chtimes(newFile, newTime, newTime))

	require.Equal(t, newToken, c.jupyterToken())
	require.Equal(t, []string{newToken, oldToken}, c.jupyterTokens())

	// 3. Symlinked runtime file, permissive runtime dir, and mismatched owner UID are rejected.
	targetFile := filepath.Join(runtimeRoot, "external-target.json")
	targetData, err := json.Marshal(jupyterRuntimeServerInfo{Port: port, Token: "symlink-token"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(targetFile, targetData, 0o600))
	symlinkDir := filepath.Join(runtimeRoot, "opensandbox-jupyter.symlink")
	require.NoError(t, os.MkdirAll(symlinkDir, 0o700))
	require.NoError(t, os.Symlink(targetFile, filepath.Join(symlinkDir, "jpserver-4.json")))

	worldDirFile := writeJupyterServerFile(t, runtimeRoot, "opensandbox-jupyter.worlddir", "jpserver-5.json", jupyterRuntimeServerInfo{
		Port:  port,
		Token: "world-dir-token",
	})
	require.NoError(t, os.Chmod(filepath.Dir(worldDirFile), 0o755))

	require.Equal(t, []string{newToken, oldToken}, c.jupyterTokens())

	otherUID := os.Geteuid() + 1
	c.jupyterRuntimeUID = &otherUID
	if _, hasUID := runtimeFileOwnerUID(mustStat(t, newFile)); hasUID {
		require.Empty(t, c.jupyterTokens())
	}
	c.jupyterRuntimeUID = nil

	// 4. Explicit token always takes precedence over runtime discovery.
	configured := NewController(baseURL, initToken)
	configured.jupyterRuntimeRoot = runtimeRoot
	require.Equal(t, initToken, configured.jupyterToken())
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info
}

func TestCreateContextAndRunJupyter_FastFailWhenTokenMissing(t *testing.T) {
	c := NewController("http://127.0.0.1:44771", "")
	c.jupyterRuntimeRoot = t.TempDir()

	start := time.Now()
	_, err := c.CreateContext(context.Background(), &CreateContextRequest{
		Language: Python,
		Cwd:      t.TempDir(),
	})
	require.ErrorIs(t, err, errJupyterNotConfigured)
	require.Less(t, time.Since(start), 200*time.Millisecond)

	start = time.Now()
	err = c.Execute(&ExecuteCodeRequest{
		Language: Python,
		Code:     "print(1)",
	})
	require.ErrorIs(t, err, errJupyterNotConfigured)
	require.Less(t, time.Since(start), 200*time.Millisecond)
}

func TestCreateContext_SkipsNewerUnauthenticatedDecoyRuntimeFile(t *testing.T) {
	const (
		validToken = "valid-jupyter-runtime-token"
		decoyToken = "newer-decoy-runtime-token"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+validToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/kernelspecs"):
			_, _ = w.Write([]byte(`{"default":"python","kernelspecs":{"python":{"name":"python","spec":{"language":"python","display_name":"Python"}}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/sessions"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sess-after-decoy","kernel":{"id":"kernel-1","name":"python"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/kernels"):
			_, _ = w.Write([]byte(`[{"id":"kernel-1","name":"python"}]`))
		default:
			require.Failf(t, "unexpected request", "%s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	runtimeRoot := t.TempDir()
	port := jupyterPortFromBaseURL(server.URL)

	realFile := writeJupyterServerFile(t, runtimeRoot, "opensandbox-jupyter.real", "jpserver-1.json", jupyterRuntimeServerInfo{
		Port:  port,
		Token: validToken,
	})
	oldTime := time.Now().Add(-10 * time.Second)
	require.NoError(t, os.Chtimes(realFile, oldTime, oldTime))

	decoyFile := writeJupyterServerFile(t, runtimeRoot, "opensandbox-jupyter.decoy", "jpserver-2.json", jupyterRuntimeServerInfo{
		Port:  port,
		Token: decoyToken,
	})
	newTime := time.Now().Add(-1 * time.Second)
	require.NoError(t, os.Chtimes(decoyFile, newTime, newTime))

	c := NewController(server.URL, "")
	c.jupyterRuntimeRoot = runtimeRoot

	sessionID, err := c.CreateContext(context.Background(), &CreateContextRequest{
		Language: Python,
		Cwd:      t.TempDir(),
	})
	require.NoError(t, err)
	require.Equal(t, "sess-after-decoy", sessionID)
}

func TestKernelWaitingBackoffKeepsWaitingForJupyterStartup(t *testing.T) {
	backoff := kernelWaitingBackoff
	backoff.Jitter = 0
	var total time.Duration
	for backoff.Steps > 0 {
		delay := backoff.Step()
		require.LessOrEqual(t, delay, 5*time.Second, "a single wait should stay short")
		total += delay
	}
	require.GreaterOrEqual(t, total, 4*time.Minute, "a slow Jupyter start should still be waited for")
}

func TestCreateContextStopsRetryingWhenContextIsDone(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	c := NewController(server.URL, "token")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.CreateContext(ctx, &CreateContextRequest{Language: Python, Cwd: t.TempDir()})
	// retry.OnError reports the last Jupyter error once ctx stops the retries.
	require.ErrorContains(t, err, "503")
	require.Less(t, time.Since(start), 3*time.Second)
	require.Equal(t, int32(1), attempts.Load())
}

func TestCreateContextStopsWhenJupyterNeverResponds(t *testing.T) {
	original := jupyterRequestTimeout
	jupyterRequestTimeout = 100 * time.Millisecond
	defer func() { jupyterRequestTimeout = original }()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)

	c := NewController(server.URL, "token")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.CreateContext(ctx, &CreateContextRequest{Language: Python, Cwd: t.TempDir()})
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("CreateContext kept waiting on a Jupyter request that never answered")
	}
}

func TestCreateContextDeletesSessionWhenKernelCheckFails(t *testing.T) {
	var created, deleted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/kernelspecs"):
			_, _ = w.Write([]byte(`{"default":"python","kernelspecs":{"python":{"name":"python","spec":{"language":"python","display_name":"Python"}}}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/sessions"):
			created.Add(1)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"sess-1","kernel":{"id":"kernel-1","name":"python"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/kernels"):
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/api/sessions/sess-1"):
			deleted.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := NewController(server.URL, "token")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := c.CreateContext(ctx, &CreateContextRequest{Language: Python, Cwd: t.TempDir()})
	require.Error(t, err)
	require.Equal(t, int32(1), created.Load())
	require.Equal(t, created.Load(), deleted.Load(), "every session created by a failed attempt is deleted")
}
