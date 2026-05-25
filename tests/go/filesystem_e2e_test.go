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

package e2e

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func TestFilesystem_GetFileInfo(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	info, err := sb.GetFileInfo(ctx, "/etc/os-release")
	require.NoError(t, err)

	fi, ok := info["/etc/os-release"]
	require.True(t, ok, "expected /etc/os-release in result")
	require.NotZero(t, fi.Size, "expected non-zero file size")
	t.Logf("File info: path=%s size=%d owner=%s", fi.Path, fi.Size, fi.Owner)
}

func TestFilesystem_WriteReadDelete(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	exec, err := sb.RunCommand(ctx, `echo "go-e2e-content" > /tmp/test-rw.txt`, nil)
	require.NoError(t, err)
	if exec.ExitCode != nil {
		require.Equal(t, 0, *exec.ExitCode, "write exit code")
	}

	exec, err = sb.RunCommand(ctx, "cat /tmp/test-rw.txt", nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "go-e2e-content")

	info, err := sb.GetFileInfo(ctx, "/tmp/test-rw.txt")
	require.NoError(t, err)
	_, ok := info["/tmp/test-rw.txt"]
	require.True(t, ok, "file not found via GetFileInfo")

	err = sb.DeleteFiles(ctx, []string{"/tmp/test-rw.txt"})
	require.NoError(t, err)

	exec, err = sb.RunCommand(ctx, "test -f /tmp/test-rw.txt && echo exists || echo gone", nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "gone")
	t.Log("Write/Read/Delete cycle passed")
}

func TestFilesystem_MoveFiles(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	sb.RunCommand(ctx, `echo "move-me" > /tmp/move-src.txt`, nil)

	err := sb.MoveFiles(ctx, opensandbox.MoveRequest{
		{Src: "/tmp/move-src.txt", Dest: "/tmp/move-dst.txt"},
	})
	require.NoError(t, err)

	exec, err := sb.RunCommand(ctx, "cat /tmp/move-dst.txt", nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "move-me")
	t.Log("MoveFiles passed")
}

func TestFilesystem_Directories(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	err := sb.CreateDirectory(ctx, "/tmp/test-dir-e2e", 755)
	require.NoError(t, err)

	exec, err := sb.RunCommand(ctx, "test -d /tmp/test-dir-e2e && echo yes || echo no", nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "yes")

	err = sb.DeleteDirectory(ctx, "/tmp/test-dir-e2e")
	require.NoError(t, err)
	t.Log("Directory create/delete passed")
}

func TestFilesystem_SearchFiles(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	results, err := sb.SearchFiles(ctx, "/etc", "*.conf")
	require.NoError(t, err)
	t.Logf("Found %d files matching *.conf in /etc", len(results))
}

func TestFilesystem_DownloadFile(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	rc, err := sb.DownloadFile(ctx, "/etc/os-release", "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NotEmpty(t, data, "downloaded file is empty")
	t.Logf("Downloaded %d bytes", len(data))
}

func TestFilesystem_UploadAndDownloadFile(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	tmp, err := os.CreateTemp("", "opensandbox-upload-*")
	require.NoError(t, err)
	defer os.Remove(tmp.Name())

	content := []byte("go-e2e-upload-download")
	_, err = tmp.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmp.Close())

	remotePath := "/tmp/go-e2e-upload-download.txt"
	up, err := os.Open(tmp.Name())
	require.NoError(t, err)
	defer up.Close()
	require.NoError(t, sb.UploadFile(ctx, up, opensandbox.UploadFileOptions{
		FileName: "go-e2e-upload-download.txt",
		Metadata: opensandbox.FileMetadata{Path: remotePath},
	}))
	t.Cleanup(func() {
		_ = sb.DeleteFiles(context.Background(), []string{remotePath})
	})

	rc, err := sb.DownloadFile(ctx, remotePath, "")
	require.NoError(t, err)
	defer rc.Close()

	downloaded, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, string(content), string(downloaded))
}

func TestFilesystem_SetPermissions(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	_, err := sb.RunCommand(ctx, `echo "perm-test" > /tmp/perm-e2e.txt`, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.DeleteFiles(context.Background(), []string{"/tmp/perm-e2e.txt"}) })

	err = sb.SetPermissions(ctx, opensandbox.PermissionsRequest{
		"/tmp/perm-e2e.txt": {Mode: 644},
	})
	require.NoError(t, err)

	exec, err := sb.RunCommand(ctx, `stat -c "%a" /tmp/perm-e2e.txt || stat -f "%Lp" /tmp/perm-e2e.txt`, nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "644")
}

func TestFilesystem_ReplaceInFiles(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	_, err := sb.RunCommand(ctx, `echo "hello localhost" > /tmp/replace-e2e.txt`, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.DeleteFiles(context.Background(), []string{"/tmp/replace-e2e.txt"}) })

	err = sb.ReplaceInFiles(ctx, opensandbox.ReplaceRequest{
		"/tmp/replace-e2e.txt": {Old: "localhost", New: "example.com"},
	})
	require.NoError(t, err)

	exec, err := sb.RunCommand(ctx, "cat /tmp/replace-e2e.txt", nil)
	require.NoError(t, err)
	require.Contains(t, exec.Text(), "example.com")
}
