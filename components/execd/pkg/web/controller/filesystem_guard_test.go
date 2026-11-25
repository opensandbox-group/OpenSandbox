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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func withTestFilesystemGuard(t *testing.T, policy FilesystemGuardPolicy) {
	t.Helper()
	prev := activeFilesystemGuard.Load()
	require.NoError(t, InitFilesystemGuard(policy))
	t.Cleanup(func() {
		activeFilesystemGuard.Store(prev)
	})
}

func assertForbiddenResponse(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodePermissionDenied, resp.Code)
}

func buildUploadMultipart(t *testing.T, targetPath string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metaPart, err := writer.CreateFormFile("metadata", "metadata.json")
	require.NoError(t, err)
	require.NoError(t, json.NewEncoder(metaPart).Encode(map[string]any{
		"path": targetPath,
		"mode": 644,
	}))
	filePart, err := writer.CreateFormFile("file", "payload.txt")
	require.NoError(t, err)
	_, err = filePart.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return &body, writer.FormDataContentType()
}

func TestFilesystemGuardBlocksDefaultPseudoAndPlatformPaths(t *testing.T) {
	withTestFilesystemGuard(t, FilesystemGuardPolicy{})

	blocked := []string{
		"/proc",
		"/proc/1/environ",
		"/proc/self/environ",
		"/proc/1/root/etc/passwd",
		"/sys",
		"/sys/kernel/hostname",
		"/sys/fs/cgroup",
		"/sys/fs/cgroup/memory.max",
		"/dev",
		"/dev/null",
		"/dev/pts/0",
		"/var/lib/execd",
		"/var/lib/execd/isolation/sess1/upper/secret.txt",
		"/etc/execd",
		"/etc/execd/isolation.toml",
		"/etc/opensandbox",
		"/etc/opensandbox/seccomp.bpf",
		"/opt/opensandbox",
		"/opt/opensandbox/bootstrap.sh",
		"/var/log/opensandbox",
		"/var/log/opensandbox/ebpf-audit.jsonl",
		"/workspace/../proc/1/environ",
		"/tmp/../../etc/opensandbox/token",
	}
	for _, p := range blocked {
		err := checkFilesystemPath(p)
		require.Truef(t, errors.Is(err, errForbiddenPath), "expected %q to be blocked, got %v", p, err)
	}

	allowed := []string{
		"/workspace/app/main.go",
		"/tmp/scratch.txt",
		"/proc_custom/file.txt",
		"/system/file.txt",
		"/developer/notes.md",
		"/etc/execd_other/file.txt",
		"/opt/opensandbox-user/app.py",
	}
	for _, p := range allowed {
		require.NoErrorf(t, checkFilesystemPath(p), "expected %q to be allowed", p)
	}
}

func TestFilesystemGuardSymlinkResolution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink test")
	}
	withTestFilesystemGuard(t, FilesystemGuardPolicy{})

	ws := t.TempDir()
	okFile := filepath.Join(ws, "ok.txt")
	require.NoError(t, os.WriteFile(okFile, []byte("ok"), 0o644))

	okLink := filepath.Join(ws, "ok_link")
	require.NoError(t, os.Symlink(okFile, okLink))
	require.NoError(t, checkFilesystemPath(okLink))

	procLink := filepath.Join(ws, "proc_leak")
	require.NoError(t, os.Symlink("/proc/1/environ", procLink))
	require.ErrorIs(t, checkFilesystemPath(procLink), errForbiddenPath)

	danglingBlocked := filepath.Join(ws, "dangling_opt")
	require.NoError(t, os.Symlink("/opt/opensandbox/nonexistent-token", danglingBlocked))
	require.ErrorIs(t, checkFilesystemPath(danglingBlocked), errForbiddenPath)

	dirLink := filepath.Join(ws, "etc_link")
	require.NoError(t, os.Symlink("/etc/opensandbox", dirLink))
	require.ErrorIs(t, checkFilesystemPath(filepath.Join(dirLink, "sub", "file.toml")), errForbiddenPath)

	// Multi-hop relative symlink chain resolving to a blocked path.
	sub := filepath.Join(ws, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	hop2 := filepath.Join(ws, "hop2")
	require.NoError(t, os.Symlink("/var/lib/execd/isolation", hop2))
	hop1 := filepath.Join(sub, "hop1")
	require.NoError(t, os.Symlink("../hop2", hop1))
	require.ErrorIs(t, checkFilesystemPath(filepath.Join(hop1, "session/file")), errForbiddenPath)
}

func TestFilesystemGuardDynamicPathsAndDenyGlobs(t *testing.T) {
	tmp := t.TempDir()
	customUpper := filepath.Join(tmp, "custom-upper")
	customConfig := filepath.Join(tmp, "custom-isolation.toml")
	require.NoError(t, os.MkdirAll(customUpper, 0o755))
	require.NoError(t, os.WriteFile(customConfig, []byte("upper_root = \"/tmp\""), 0o600))

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{customUpper, customConfig},
		DenyGlobs: []string{
			"**/.kube/config",
			"**/.aws/credentials",
			"**/.git-credentials",
		},
	})

	require.ErrorIs(t, checkFilesystemPath(customUpper), errForbiddenPath)
	require.ErrorIs(t, checkFilesystemPath(filepath.Join(customUpper, "sess/file")), errForbiddenPath)
	require.ErrorIs(t, checkFilesystemPath(customConfig), errForbiddenPath)
	require.ErrorIs(t, checkFilesystemPath(filepath.Join(tmp, "home/.kube/config")), errForbiddenPath)
	require.ErrorIs(t, checkFilesystemPath(filepath.Join(tmp, "user/.aws/credentials")), errForbiddenPath)
	require.ErrorIs(t, checkFilesystemPath(filepath.Join(tmp, ".git-credentials")), errForbiddenPath)
	require.NoError(t, checkFilesystemPath(filepath.Join(tmp, "home/readme.md")))

	if runtime.GOOS != "windows" {
		credDir := filepath.Join(tmp, "home", ".aws")
		require.NoError(t, os.MkdirAll(credDir, 0o755))
		credFile := filepath.Join(credDir, "credentials")
		require.NoError(t, os.WriteFile(credFile, []byte("secret"), 0o600))
		credLink := filepath.Join(tmp, "aws-link")
		require.NoError(t, os.Symlink(credFile, credLink))
		require.ErrorIs(t, checkFilesystemPath(credLink), errForbiddenPath)
	}

	require.Error(t, InitFilesystemGuard(FilesystemGuardPolicy{DenyGlobs: []string{"[invalid"}}))
}

func TestFilesystemControllerBlockedPathsReturn403(t *testing.T) {
	tmp := t.TempDir()
	blockedDir := filepath.Join(tmp, "internal-platform")
	require.NoError(t, os.MkdirAll(blockedDir, 0o755))
	blockedFile := filepath.Join(blockedDir, "secret.txt")
	require.NoError(t, os.WriteFile(blockedFile, []byte("top-secret\nline2\n"), 0o600))

	workspace := filepath.Join(tmp, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	normalFile := filepath.Join(workspace, "normal.txt")
	require.NoError(t, os.WriteFile(normalFile, []byte("hello"), 0o644))

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{blockedDir},
	})

	t.Run("GetFilesInfo", func(t *testing.T) {
		for _, target := range []string{"/proc/1/environ", blockedFile} {
			ctrl, rec := newFilesystemController(t, http.MethodGet, "/files/info?path="+url.QueryEscape(target), nil)
			ctrl.GetFilesInfo()
			assertForbiddenResponse(t, rec)
		}
	})

	t.Run("DownloadFile", func(t *testing.T) {
		for _, q := range []string{
			"/files/download?path=" + url.QueryEscape("/proc/1/environ") + "&offset=1",
			"/files/download?path=" + url.QueryEscape(blockedFile),
			"/files/download?path=" + url.QueryEscape(blockedFile) + "&offset=1&limit=1",
		} {
			ctrl, rec := newFilesystemController(t, http.MethodGet, q, nil)
			ctrl.DownloadFile()
			assertForbiddenResponse(t, rec)
			require.NotContains(t, rec.Body.String(), "top-secret")
		}
	})

	t.Run("RemoveFiles", func(t *testing.T) {
		ctrl, rec := newFilesystemController(t, http.MethodDelete, "/files?path="+url.QueryEscape(blockedFile), nil)
		ctrl.RemoveFiles()
		assertForbiddenResponse(t, rec)
		_, err := os.Stat(blockedFile)
		require.NoError(t, err, "blocked file must not be deleted")
	})

	t.Run("ChmodFiles", func(t *testing.T) {
		body, err := json.Marshal(map[string]model.Permission{blockedFile: {Mode: 777}})
		require.NoError(t, err)
		ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/permissions", body)
		ctrl.ChmodFiles()
		assertForbiddenResponse(t, rec)
	})

	t.Run("RenameFiles", func(t *testing.T) {
		for _, item := range []model.RenameFileItem{
			{Src: blockedFile, Dest: filepath.Join(workspace, "stolen.txt")},
			{Src: normalFile, Dest: filepath.Join(blockedDir, "injected.txt")},
		} {
			body, err := json.Marshal([]model.RenameFileItem{item})
			require.NoError(t, err)
			ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/mv", body)
			ctrl.RenameFiles()
			assertForbiddenResponse(t, rec)
		}
	})

	t.Run("ReplaceContent", func(t *testing.T) {
		body, err := json.Marshal(map[string]model.ReplaceFileContentItem{
			blockedFile: {Old: "top-secret", New: "tampered"},
		})
		require.NoError(t, err)
		ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/replace", body)
		ctrl.ReplaceContent()
		assertForbiddenResponse(t, rec)
		data, err := os.ReadFile(blockedFile)
		require.NoError(t, err)
		require.Equal(t, "top-secret\nline2\n", string(data))
	})

	t.Run("UploadFile direct and via symlink", func(t *testing.T) {
		targets := []string{filepath.Join(blockedDir, "new.txt")}
		if runtime.GOOS != "windows" {
			linkPath := filepath.Join(workspace, "upload-link.txt")
			require.NoError(t, os.Symlink(blockedFile, linkPath))
			targets = append(targets, linkPath)
		}
		for _, target := range targets {
			body, contentType := buildUploadMultipart(t, target, []byte("overwritten"))
			ctx, rec := newTestContext(http.MethodPost, "/files/upload", body.Bytes())
			ctx.Request.Header.Set("Content-Type", contentType)
			ctrl := NewFilesystemController(ctx)
			ctrl.UploadFile()
			assertForbiddenResponse(t, rec)
		}
		data, err := os.ReadFile(blockedFile)
		require.NoError(t, err)
		require.Equal(t, "top-secret\nline2\n", string(data))
		_, err = os.Stat(filepath.Join(blockedDir, "new.txt"))
		require.True(t, os.IsNotExist(err))
	})

	t.Run("MakeDirs", func(t *testing.T) {
		newBlockedDir := filepath.Join(blockedDir, "sub", "dir")
		body, err := json.Marshal(map[string]model.Permission{newBlockedDir: {Mode: 755}})
		require.NoError(t, err)
		ctrl, rec := newFilesystemController(t, http.MethodPost, "/directories", body)
		ctrl.MakeDirs()
		assertForbiddenResponse(t, rec)
		_, err = os.Stat(newBlockedDir)
		require.True(t, os.IsNotExist(err))
	})

	t.Run("RemoveDirs", func(t *testing.T) {
		ctrl, rec := newFilesystemController(t, http.MethodDelete, "/directories?path="+url.QueryEscape(blockedDir), nil)
		ctrl.RemoveDirs()
		assertForbiddenResponse(t, rec)
		_, err := os.Stat(blockedDir)
		require.NoError(t, err, "blocked dir must not be deleted")
	})

	t.Run("ListDirectory", func(t *testing.T) {
		for _, target := range []string{"/proc", blockedDir} {
			ctrl, rec := newFilesystemController(t, http.MethodGet, "/directories/list?path="+url.QueryEscape(target), nil)
			ctrl.ListDirectory()
			assertForbiddenResponse(t, rec)
		}
	})

	t.Run("SearchFiles", func(t *testing.T) {
		for _, target := range []string{"/proc", blockedDir} {
			ctrl, rec := newFilesystemController(t, http.MethodGet, "/files/search?path="+url.QueryEscape(target), nil)
			ctrl.SearchFiles()
			assertForbiddenResponse(t, rec)
		}
	})
}

func TestFilesystemControllerWalkSkipsBlockedSubtrees(t *testing.T) {
	root := t.TempDir()
	allowedDir := filepath.Join(root, "workspace")
	blockedDir := filepath.Join(root, "platform")
	awsDir := filepath.Join(allowedDir, ".aws")
	require.NoError(t, os.MkdirAll(awsDir, 0o755))
	require.NoError(t, os.MkdirAll(blockedDir, 0o755))

	okFile := filepath.Join(allowedDir, "hello.txt")
	secretFile := filepath.Join(blockedDir, "secret.txt")
	awsCred := filepath.Join(awsDir, "credentials")
	require.NoError(t, os.WriteFile(okFile, []byte("hello"), 0o644))
	require.NoError(t, os.WriteFile(secretFile, []byte("secret"), 0o600))
	require.NoError(t, os.WriteFile(awsCred, []byte("aws-secret"), 0o600))

	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink(secretFile, filepath.Join(allowedDir, "leak")))
	}

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{blockedDir},
		DenyGlobs:         []string{"**/.aws/credentials"},
	})

	t.Run("ListDirectory skips blocked subtrees and files", func(t *testing.T) {
		ctrl, rec := newFilesystemController(
			t,
			http.MethodGet,
			fmt.Sprintf("/directories/list?path=%s&depth=4", url.QueryEscape(root)),
			nil,
		)
		ctrl.ListDirectory()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var entries []model.FileInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
		paths := make([]string, 0, len(entries))
		for _, e := range entries {
			paths = append(paths, e.Path)
		}
		require.Contains(t, paths, okFile)
		require.NotContains(t, paths, blockedDir)
		require.NotContains(t, paths, secretFile)
		require.NotContains(t, paths, awsCred)
		require.NotContains(t, paths, filepath.Join(allowedDir, "leak"))
	})

	t.Run("SearchFiles skips blocked subtrees and files", func(t *testing.T) {
		ctrl, rec := newFilesystemController(
			t,
			http.MethodGet,
			fmt.Sprintf("/files/search?path=%s&pattern=**", url.QueryEscape(root)),
			nil,
		)
		ctrl.SearchFiles()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var entries []model.FileInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
		paths := make([]string, 0, len(entries))
		for _, e := range entries {
			paths = append(paths, e.Path)
		}
		require.Equal(t, []string{okFile}, paths)
	})
}

func TestFilesystemGuardSymlinkDotDotTraversalBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink test")
	}
	tmp := t.TempDir()
	blockedDir := filepath.Join(tmp, "blocked")
	require.NoError(t, os.MkdirAll(blockedDir, 0o755))
	secretFile := filepath.Join(blockedDir, "keep.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("keep"), 0o600))

	ws := filepath.Join(tmp, "workspace")
	require.NoError(t, os.MkdirAll(ws, 0o755))
	linkPath := filepath.Join(ws, "l")
	require.NoError(t, os.Symlink(blockedDir, linkPath))

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{blockedDir},
	})

	// S1: DELETE /directories with symlink/.. must not remove the blocked tree.
	dotDotDelete := linkPath + "/../blocked"
	ctrl, _ := newFilesystemController(t, http.MethodDelete, "/directories?path="+url.QueryEscape(dotDotDelete), nil)
	ctrl.RemoveDirs()
	_, err := os.Stat(secretFile)
	require.NoError(t, err, "blocked directory must survive symlink/.. RemoveDirs")

	// S2: POST /files/upload with symlink/.. must not write into the blocked directory.
	dotDotUpload := linkPath + "/../blocked/injected.txt"
	body, contentType := buildUploadMultipart(t, dotDotUpload, []byte("injected"))
	ctx, _ := newTestContext(http.MethodPost, "/files/upload", body.Bytes())
	ctx.Request.Header.Set("Content-Type", contentType)
	NewFilesystemController(ctx).UploadFile()
	_, err = os.Stat(filepath.Join(blockedDir, "injected.txt"))
	require.True(t, os.IsNotExist(err), "upload must not create file inside blocked dir via symlink/..")
}

func TestFilesystemGuardPolicyNormalizesRelativePathsAndFailClosedHops(t *testing.T) {
	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{"relative/secret.txt"},
	})
	exported := CurrentFilesystemGuardPolicy()
	require.Len(t, exported.ExtraBlockedPaths, 1)
	require.True(t, filepath.IsAbs(exported.ExtraBlockedPaths[0]), "propagated path must be absolute: %q", exported.ExtraBlockedPaths[0])

	if runtime.GOOS != "windows" {
		ws := t.TempDir()
		loopA := filepath.Join(ws, "a")
		loopB := filepath.Join(ws, "b")
		require.NoError(t, os.Symlink(loopB, loopA))
		require.NoError(t, os.Symlink(loopA, loopB))
		require.ErrorIs(t, checkFilesystemPath(loopA), errForbiddenPath)
	}
}

func TestFilesystemGuardWindowsNormalization(t *testing.T) {
	g, err := buildFilesystemGuardForOS(FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{
			`C:\OpenSandbox\sub\..\secret\.\\`,
			`\\fileserver\share\a\..\vault\\`,
		},
		DenyGlobs: []string{"**/.kube/config", "**/.aws/credentials", "**/.git-credentials"},
	}, true)
	require.NoError(t, err)

	require.True(t, g.matchesPath(`c:\opensandbox\SECRET`))
	require.True(t, g.matchesPath(`C:\OpenSandbox\secret\token.json`))
	require.True(t, g.matchesPath(`\\FILESERVER\Share\vault\key.pem`))
	require.True(t, g.matchesPath(`C:\Users\Alice\.aws\credentials`))
	require.True(t, g.matchesPath(`C:\Users\Alice\.KUBE\config`))
	require.True(t, g.matchesPath(`C:\Users\Alice\.git-credentials`))
	require.False(t, g.matchesPath(`C:\Users\Alice\workspace\main.go`))
}

func TestFilesystemGuardSymlinkTargetWithNestedDotDotBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink test")
	}
	tmp := t.TempDir()
	blockedDir := filepath.Join(tmp, "blocked")
	require.NoError(t, os.MkdirAll(blockedDir, 0o755))
	secretFile := filepath.Join(blockedDir, "secret.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("secret"), 0o600))

	ws := filepath.Join(tmp, "workspace")
	require.NoError(t, os.MkdirAll(ws, 0o755))
	dirLink := filepath.Join(ws, "dirlink")
	require.NoError(t, os.Symlink(blockedDir, dirLink))
	// Symlink target contains "<symlink>/.." which lexical Clean would collapse
	// to "<ws>/blocked" (allowed), while the kernel resolves dirlink first.
	xLink := filepath.Join(ws, "x")
	require.NoError(t, os.Symlink(dirLink+"/../blocked", xLink))

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{blockedDir},
	})

	targetViaX := filepath.Join(xLink, "secret.txt")
	require.ErrorIs(t, checkFilesystemPath(targetViaX), errForbiddenPath)

	// GET /files/info
	ctrlInfo, recInfo := newFilesystemController(t, http.MethodGet, "/files/info?path="+url.QueryEscape(targetViaX), nil)
	ctrlInfo.GetFilesInfo()
	assertForbiddenResponse(t, recInfo)

	// POST /files/permissions
	chmodBody, err := json.Marshal(map[string]model.Permission{targetViaX: {Mode: 777}})
	require.NoError(t, err)
	ctrlChmod, recChmod := newFilesystemController(t, http.MethodPost, "/files/permissions", chmodBody)
	ctrlChmod.ChmodFiles()
	assertForbiddenResponse(t, recChmod)

	// POST /files/mv
	mvBody, err := json.Marshal([]model.RenameFileItem{{Src: targetViaX, Dest: filepath.Join(ws, "exfil.txt")}})
	require.NoError(t, err)
	ctrlMv, recMv := newFilesystemController(t, http.MethodPost, "/files/mv", mvBody)
	ctrlMv.RenameFiles()
	assertForbiddenResponse(t, recMv)

	// DELETE /files
	ctrlDel, recDel := newFilesystemController(t, http.MethodDelete, "/files?path="+url.QueryEscape(targetViaX), nil)
	ctrlDel.RemoveFiles()
	assertForbiddenResponse(t, recDel)

	// POST /files/upload
	upBody, upCT := buildUploadMultipart(t, filepath.Join(xLink, "injected.txt"), []byte("bad"))
	ctxUp, recUp := newTestContext(http.MethodPost, "/files/upload", upBody.Bytes())
	ctxUp.Request.Header.Set("Content-Type", upCT)
	NewFilesystemController(ctxUp).UploadFile()
	assertForbiddenResponse(t, recUp)

	// POST /directories & DELETE /directories
	mkBody, err := json.Marshal(map[string]model.Permission{filepath.Join(xLink, "sub"): {Mode: 755}})
	require.NoError(t, err)
	ctrlMk, recMk := newFilesystemController(t, http.MethodPost, "/directories", mkBody)
	ctrlMk.MakeDirs()
	assertForbiddenResponse(t, recMk)

	ctrlRm, recRm := newFilesystemController(t, http.MethodDelete, "/directories?path="+url.QueryEscape(xLink), nil)
	ctrlRm.RemoveDirs()
	assertForbiddenResponse(t, recRm)

	// Verify secret file is intact and untouched.
	info, err := os.Stat(secretFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestFilesystemGuardBlocksAncestorDeleteAndRename(t *testing.T) {
	tmp := t.TempDir()
	parentDir := filepath.Join(tmp, "parent")
	blockedDir := filepath.Join(parentDir, "blocked")
	require.NoError(t, os.MkdirAll(blockedDir, 0o755))
	secretFile := filepath.Join(blockedDir, "secret.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("secret"), 0o600))

	ws := filepath.Join(tmp, "workspace")
	require.NoError(t, os.MkdirAll(ws, 0o755))

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{blockedDir},
	})

	// DELETE /directories on ancestor of blocked root or filesystem root -> 403.
	for _, target := range []string{parentDir, string(filepath.Separator)} {
		ctrl, rec := newFilesystemController(t, http.MethodDelete, "/directories?path="+url.QueryEscape(target), nil)
		ctrl.RemoveDirs()
		assertForbiddenResponse(t, rec)
	}
	_, err := os.Stat(secretFile)
	require.NoError(t, err, "ancestor RemoveDirs must not remove blocked subtree")

	// POST /files/mv relocating ancestor of blocked root or filesystem root -> 403.
	for _, src := range []string{parentDir, string(filepath.Separator)} {
		body, err := json.Marshal([]model.RenameFileItem{{
			Src:  src,
			Dest: filepath.Join(ws, "relocated"),
		}})
		require.NoError(t, err)
		ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/mv", body)
		ctrl.RenameFiles()
		assertForbiddenResponse(t, rec)
	}
	_, err = os.Stat(secretFile)
	require.NoError(t, err, "ancestor RenameFiles must not relocate blocked subtree")
	_, err = os.Stat(filepath.Join(ws, "relocated"))
	require.True(t, os.IsNotExist(err))
}

func TestFilesystemGuardBlocksAnchoredGlobAncestorMoveAndDelete(t *testing.T) {
	tmp := t.TempDir()
	aliceDir := filepath.Join(tmp, "home", "alice")
	awsDir := filepath.Join(aliceDir, ".aws")
	require.NoError(t, os.MkdirAll(awsDir, 0o755))
	credFile := filepath.Join(awsDir, "credentials")
	require.NoError(t, os.WriteFile(credFile, []byte("TOP-SECRET-CRED"), 0o600))

	ws := filepath.Join(tmp, "workspace")
	require.NoError(t, os.MkdirAll(ws, 0o755))

	anchoredGlob := filepath.Join(tmp, "home", "*", ".aws", "credentials")
	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		DenyGlobs: []string{anchoredGlob},
	})

	// POST /files/mv moving an ancestor of an anchored-glob file -> 403.
	body, err := json.Marshal([]model.RenameFileItem{{
		Src:  aliceDir,
		Dest: filepath.Join(ws, "alice"),
	}})
	require.NoError(t, err)
	ctrlMv, recMv := newFilesystemController(t, http.MethodPost, "/files/mv", body)
	ctrlMv.RenameFiles()
	assertForbiddenResponse(t, recMv)

	// DELETE /directories on an ancestor of an anchored-glob file -> 403.
	ctrlRm, recRm := newFilesystemController(t, http.MethodDelete, "/directories?path="+url.QueryEscape(aliceDir), nil)
	ctrlRm.RemoveDirs()
	assertForbiddenResponse(t, recRm)

	_, err = os.Stat(credFile)
	require.NoError(t, err, "glob-protected credential file must remain intact")
}
