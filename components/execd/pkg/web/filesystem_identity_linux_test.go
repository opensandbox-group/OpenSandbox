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

//go:build linux

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == FilesystemWorkerArg {
		if err := RunFilesystemWorker(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func identityTestRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	registerFilesystemRoutes(router.Group(""))
	registerFilesystemRoutes(router.Group("/v1/filesystem/:uid/:gid", filesystemIdentityMiddleware()))
	return router
}

func identityFixture(t *testing.T) (string, string, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root with CAP_SETUID/CAP_SETGID")
	}
	// t.TempDir's ancestors can be mode 0700, preventing either worker from
	// traversing them. Use a directly accessible temporary parent instead.
	root, err := os.MkdirTemp("", "filesystem-identity-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	require.NoError(t, os.Chmod(root, 0755))
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for i, dir := range []string{a, b} {
		require.NoError(t, os.Mkdir(dir, 0750))
		require.NoError(t, os.Chown(dir, 41001+i, 41000))
		path := filepath.Join(dir, "file.txt")
		require.NoError(t, os.WriteFile(path, []byte("original\nsecond line\n"), 0640))
		require.NoError(t, os.Chown(path, 41001+i, 41000))
	}
	return root, a, b
}

func identityRequest(router http.Handler, uid int, method, route string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, fmt.Sprintf("/v1/filesystem/%d/41000%s", uid, route), body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response
}

func identityUpload(t *testing.T, router http.Handler, uid int, path string, data []byte, owner string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadata, err := writer.CreateFormFile("metadata", "metadata.json")
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(metadata).Encode(map[string]any{"path": path, "mode": 640, "owner": owner}))
	file, err := writer.CreateFormFile("file", "data.bin")
	require.NoError(t, err)
	_, err = file.Write(data)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return identityRequest(router, uid, http.MethodPost, "/files/upload", &body, writer.FormDataContentType())
}

func TestFilesystemIdentityPermissions(t *testing.T) {
	_, a, b := identityFixture(t)
	router := identityTestRouter()
	bfile := filepath.Join(b, "file.txt")
	for _, route := range []string{
		"/files/info?path=" + url.QueryEscape(bfile),
		"/files/search?path=" + url.QueryEscape(b),
		"/directories/list?path=" + url.QueryEscape(b),
		"/files/download?path=" + url.QueryEscape(bfile),
	} {
		response := identityRequest(router, 41001, http.MethodGet, route, nil, "")
		require.Equal(t, http.StatusOK, response.Code, "%s: %s", route, response.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/filesystem/41001/41000/files/download?path="+url.QueryEscape(bfile), nil)
	req.Header.Set("Range", "bytes=0-3")
	ranged := httptest.NewRecorder()
	router.ServeHTTP(ranged, req)
	require.Equal(t, http.StatusPartialContent, ranged.Code)
	require.Equal(t, "orig", ranged.Body.String())
	lines := identityRequest(router, 41001, http.MethodGet, "/files/download?offset=2&limit=1&path="+url.QueryEscape(bfile), nil, "")
	require.Equal(t, http.StatusOK, lines.Code)
	require.Equal(t, "second line", lines.Body.String())

	binary := []byte{0, 1, 2, 255, 10, 0}
	own := filepath.Join(a, "binary.dat")
	uploaded := identityUpload(t, router, 41001, own, binary, "")
	require.Equal(t, http.StatusOK, uploaded.Code, uploaded.Body.String())
	contents, err := os.ReadFile(own)
	require.NoError(t, err)
	require.Equal(t, binary, contents)
	stat, err := os.Stat(own)
	require.NoError(t, err)
	require.Equal(t, uint32(41001), stat.Sys().(*syscall.Stat_t).Uid)
	require.Equal(t, uint32(41000), stat.Sys().(*syscall.Stat_t).Gid)

	ownershipBody, err := json.Marshal(map[string]any{own: map[string]string{"owner": "root"}})
	require.NoError(t, err)
	ownershipResponse := identityRequest(router, 41001, http.MethodPost, "/files/permissions", bytes.NewReader(ownershipBody), "application/json")
	require.GreaterOrEqual(t, ownershipResponse.Code, 400, ownershipResponse.Body.String())
	stat, err = os.Stat(own)
	require.NoError(t, err)
	require.Equal(t, uint32(41001), stat.Sys().(*syscall.Stat_t).Uid)

	// The unknown numeric user must not inherit the root daemon's group.
	rootGroupFile := filepath.Join(filepath.Dir(a), "root-group-only")
	require.NoError(t, os.WriteFile(rootGroupFile, []byte("private"), 0640))
	require.NoError(t, os.Chown(rootGroupFile, 0, 0))
	groupResponse := identityRequest(router, 41001, http.MethodGet, "/files/download?path="+url.QueryEscape(rootGroupFile), nil, "")
	require.GreaterOrEqual(t, groupResponse.Code, 400, groupResponse.Body.String())

	// Target ownership cannot authorize opening or truncating B's file.
	deniedUpload := identityUpload(t, router, 41001, bfile, []byte("forbidden"), "root")
	require.GreaterOrEqual(t, deniedUpload.Code, 400, deniedUpload.Body.String())
	require.Contains(t, deniedUpload.Body.String(), "permission")

	denied := []struct {
		method, route string
		body          any
	}{
		{http.MethodPost, "/directories", map[string]any{filepath.Join(b, "new"): map[string]int{"mode": 755}}},
		{http.MethodPost, "/files/replace", map[string]any{bfile: map[string]string{"old": "original", "new": "forbidden"}}},
		{http.MethodPost, "/files/permissions", map[string]any{bfile: map[string]int{"mode": 777}}},
		{http.MethodPost, "/files/mv", []map[string]string{{"src": bfile, "dest": filepath.Join(a, "stolen")}}},
		{http.MethodPost, "/files/mv", []map[string]string{{"src": own, "dest": filepath.Join(b, "injected")}}},
		{http.MethodDelete, "/files?path=" + url.QueryEscape(bfile), nil},
		{http.MethodDelete, "/directories?path=" + url.QueryEscape(b), nil},
	}
	for _, test := range denied {
		data, err := json.Marshal(test.body)
		require.NoError(t, err)
		response := identityRequest(router, 41001, test.method, test.route, bytes.NewReader(data), "application/json")
		require.GreaterOrEqual(t, response.Code, 400, "%s %s: %s", test.method, test.route, response.Body.String())
	}
	contents, err = os.ReadFile(bfile)
	require.NoError(t, err)
	require.Equal(t, "original\nsecond line\n", string(contents))

	// Parent traversal and symlink targets are checked by the kernel as A.
	private := filepath.Join(b, "private")
	require.NoError(t, os.Mkdir(private, 0700))
	require.NoError(t, os.Chown(private, 41002, 41000))
	secret := filepath.Join(private, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("secret"), 0644))
	for _, route := range []string{"/files/info?path=", "/files/download?path="} {
		response := identityRequest(router, 41001, http.MethodGet, route+url.QueryEscape(secret), nil, "")
		require.GreaterOrEqual(t, response.Code, 400, response.Body.String())
	}
	link := filepath.Join(a, "link")
	require.NoError(t, os.Symlink(bfile, link))
	response := identityUpload(t, router, 41001, link, []byte("forbidden"), "")
	require.GreaterOrEqual(t, response.Code, 400, response.Body.String())

	ownText := filepath.Join(a, "file.txt")
	allowed := []struct {
		method, route string
		body          any
	}{
		{http.MethodPost, "/directories", map[string]any{filepath.Join(a, "nested", "child"): map[string]int{"mode": 750}}},
		{http.MethodPost, "/files/replace", map[string]any{ownText: map[string]string{"old": "original", "new": "changed"}}},
		{http.MethodPost, "/files/permissions", map[string]any{ownText: map[string]int{"mode": 600}}},
		{http.MethodPost, "/files/mv", []map[string]string{{"src": ownText, "dest": filepath.Join(a, "renamed")}}},
		{http.MethodDelete, "/files?path=" + url.QueryEscape(filepath.Join(a, "renamed")), nil},
		{http.MethodDelete, "/directories?path=" + url.QueryEscape(filepath.Join(a, "nested")), nil},
	}
	for _, test := range allowed {
		data, err := json.Marshal(test.body)
		require.NoError(t, err)
		response := identityRequest(router, 41001, test.method, test.route, bytes.NewReader(data), "application/json")
		require.Equal(t, http.StatusOK, response.Code, "%s %s: %s", test.method, test.route, response.Body.String())
	}
	// Existing callers continue to execute as the daemon.
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/files/download?path="+url.QueryEscape(secret), nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "secret", response.Body.String())
}

func TestFilesystemIdentityConcurrentRequests(t *testing.T) {
	_, a, b := identityFixture(t)
	router := identityTestRouter()
	groups, err := os.Getgroups()
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid, dir := 41001, a
			if i%2 != 0 {
				uid, dir = 41002, b
			}
			path := filepath.Join(dir, fmt.Sprintf("request-%d", i))
			data, err := json.Marshal(map[string]any{path: map[string]int{"mode": 700}})
			if err != nil {
				t.Error(err)
				return
			}
			response := identityRequest(router, uid, http.MethodPost, "/directories", bytes.NewReader(data), "application/json")
			if response.Code != http.StatusOK {
				t.Errorf("uid %d: %d %s", uid, response.Code, response.Body.String())
				return
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Error(err)
				return
			}
			if actual := info.Sys().(*syscall.Stat_t).Uid; actual != uint32(uid) {
				t.Errorf("owner = %d, want %d", actual, uid)
			}
		}(i)
	}
	wg.Wait()
	after, err := os.Getgroups()
	require.NoError(t, err)
	require.Equal(t, groups, after)
	require.Equal(t, 0, os.Geteuid())
}

func TestFilesystemIdentityCancellation(t *testing.T) {
	_, a, _ := identityFixture(t)
	fifo := filepath.Join(a, "blocked")
	require.NoError(t, unix.Mkfifo(fifo, 0600))
	require.NoError(t, os.Chown(fifo, 41001, 41000))
	router := identityTestRouter()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/v1/filesystem/41001/41000/files/download?path="+url.QueryEscape(fifo), nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled request did not terminate the filesystem worker")
	}
}

func TestFilesystemIdentityRequiresAuthentication(t *testing.T) {
	router := NewRouter("secret-token")
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v1/filesystem/0/0/files?path=/unused", nil)
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code)
	request = httptest.NewRequest(http.MethodGet, "/v1/filesystem/4294967295/0/files/info", nil)
	request.Header.Set(model.ApiAccessTokenHeader, "secret-token")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)
}

func TestFilesystemIdentityCannotSwitch(t *testing.T) {
	const helperEnv = "OPENSANDBOX_TEST_FILESYSTEM_UNPRIVILEGED"
	if os.Getenv(helperEnv) == "1" {
		router := identityTestRouter()
		response := identityRequest(router, 0, http.MethodGet, "/files/info", nil, "")
		require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "FILESYSTEM_IDENTITY_UNAVAILABLE")
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root to launch an unprivileged test process")
	}
	cmd := exec.Command("/proc/self/exe", "-test.run=^TestFilesystemIdentityCannotSwitch$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: 41001, Gid: 41000},
	}
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
}
