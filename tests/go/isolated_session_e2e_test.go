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
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createIsolatedTestSandbox(t *testing.T) (context.Context, *opensandbox.Sandbox) {
	t.Helper()
	config := connectionConfigForStreaming(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:      getSandboxImage(),
		Extensions: map[string]string{"bootstrap.execd.isolation": "enable"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { sb.Kill(context.Background()) })

	caps, err := sb.IsolationCapabilities(ctx)
	require.NoError(t, err)
	t.Logf("Isolation capabilities: available=%v isolator=%s version=%s message=%s",
		caps.Available, caps.Isolator, caps.Version, caps.Message)
	if !caps.Available {
		t.Fatalf("Isolation NOT available: %s", caps.Message)
	}

	return ctx, sb
}

func TestIsolationCapabilities(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	caps, err := sb.IsolationCapabilities(ctx)
	require.NoError(t, err)
	assert.True(t, caps.Available)
}

func TestIsolationSessionLifecycle(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, session.SessionID())

	state, err := session.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, "active", state.Status)

	err = session.Delete(ctx)
	require.NoError(t, err)
}

func TestIsolationListSessions(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	// Create two sessions and confirm both appear in the list.
	sessionA, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer sessionA.Delete(ctx)

	sessionB, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer sessionB.Delete(ctx)

	sessions, err := sb.IsolationListSessions(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(sessions), 2)

	ids := make(map[string]opensandbox.IsolatedSessionSummary, len(sessions))
	for _, s := range sessions {
		ids[s.SessionID] = s
	}

	sumA, okA := ids[sessionA.SessionID()]
	require.True(t, okA, "sessionA should appear in list")
	assert.Equal(t, "active", sumA.Status)
	assert.False(t, sumA.CreatedAt.IsZero(), "created_at should be populated")

	_, okB := ids[sessionB.SessionID()]
	require.True(t, okB, "sessionB should appear in list")

	// After deleting a session it should no longer be listed.
	require.NoError(t, sessionB.Delete(ctx))
	sessions, err = sb.IsolationListSessions(ctx)
	require.NoError(t, err)
	for _, s := range sessions {
		assert.NotEqual(t, sessionB.SessionID(), s.SessionID, "deleted session should not be listed")
	}
}

func TestIsolationRunEcho(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "echo hello-isolation"}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), "hello-isolation")
}

func TestIsolationPIDIsolation(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "echo $$"}, nil)
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(exec.Text()))
	require.NoError(t, err)
	assert.LessOrEqual(t, pid, 2, "expected PID 1 or 2 in namespace, got %d", pid)
}

func TestIsolationRunWithEnvs(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: "echo $MY_VAR",
		Envs: map[string]string{"MY_VAR": "test-value-42"},
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), "test-value-42")
}

func TestIsolationSessionStatePersists(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	_, err = session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "export PERSIST_VAR=abc123"}, nil)
	require.NoError(t, err)

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "echo $PERSIST_VAR"}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), "abc123")
}

func TestIsolationTmpIsolation(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	sb.RunCommand(ctx, "mkdir -p /workspace", nil)

	sessionA, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/workspace", Mode: "rw"},
		Profile:   "strict",
	})
	require.NoError(t, err)
	defer sessionA.Delete(ctx)

	sessionB, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/workspace", Mode: "rw"},
		Profile:   "strict",
	})
	require.NoError(t, err)
	defer sessionB.Delete(ctx)

	_, err = sessionA.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: "echo secret > /tmp/isolated_test_file.txt",
	}, nil)
	require.NoError(t, err)

	exec, err := sessionB.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: "cat /tmp/isolated_test_file.txt 2>&1 || echo NOT_FOUND",
	}, nil)
	require.NoError(t, err)
	text := exec.Text()
	assert.True(t, strings.Contains(text, "NOT_FOUND") || strings.Contains(text, "No such file"),
		"expected /tmp isolation, got: %s", text)
}

func TestIsolationRunWithHandlers(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	var collected []string
	handlers := &opensandbox.ExecutionHandlers{
		OnStdout: func(msg opensandbox.OutputMessage) error {
			collected = append(collected, msg.Text)
			return nil
		},
	}

	_, err = session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "echo handler-test"}, handlers)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(collected, ""), "handler-test")
}

func TestIsolationFilesViaRun(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	_, err = session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "echo hello-from-sdk > /tmp/hello.txt"}, nil)
	require.NoError(t, err)

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "cat /tmp/hello.txt"}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), "hello-from-sdk")
}

func TestIsolationOverlayMode(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	marker := "overlay_marker_file.txt"
	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	_, err = session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: "echo overlay-data > /tmp/" + marker,
	}, nil)
	require.NoError(t, err)

	hostCheck, err := sb.RunCommand(ctx, "cat /tmp/"+marker+" 2>&1 || echo NOT_FOUND", nil)
	require.NoError(t, err)
	text := hostCheck.Text()
	assert.True(t, strings.Contains(text, "NOT_FOUND") || strings.Contains(text, "No such file"),
		"overlay write should not be visible on host, got: %s", text)
}

// ---------------------------------------------------------------------------
// RW filesystem API tests
// ---------------------------------------------------------------------------

func TestIsolationRWFilesUploadDownload(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_upload_%d.txt", time.Now().UnixMilli())
	content := "hello-upload-download"

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte(content)),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	rc, err := session.Files().DownloadFile(ctx, filePath, "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

func TestIsolationRWFilesInfo(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_info_%d.txt", time.Now().UnixMilli())

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("info-content")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	info, err := session.Files().GetFileInfo(ctx, filePath)
	require.NoError(t, err)
	require.Contains(t, info, filePath)
	assert.Greater(t, info[filePath].Size, int64(0))
}

func TestIsolationRWFilesSearch(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	suffix := fmt.Sprintf("%d", time.Now().UnixMilli())
	filePath := fmt.Sprintf("/tmp/test_search_%s.txt", suffix)

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("search-me")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	results, err := session.Files().SearchFiles(ctx, "/tmp", fmt.Sprintf("test_search_%s*", suffix))
	require.NoError(t, err)
	require.NotEmpty(t, results)

	found := false
	for _, fi := range results {
		if strings.Contains(fi.Path, "test_search_"+suffix) {
			found = true
			break
		}
	}
	assert.True(t, found, "expected to find uploaded file in search results")
}

func TestIsolationRWFilesMkdir(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	dirPath := fmt.Sprintf("/tmp/test_mkdir_%d", time.Now().UnixMilli())

	err = session.Files().CreateDirectory(ctx, dirPath, 755)
	require.NoError(t, err)

	info, err := session.Files().GetFileInfo(ctx, dirPath)
	require.NoError(t, err)
	require.Contains(t, info, dirPath)
	assert.Equal(t, "directory", info[dirPath].Type)
}

func TestIsolationRWFilesDelete(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_delete_%d.txt", time.Now().UnixMilli())

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("delete-me")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	err = session.Files().DeleteFiles(ctx, []string{filePath})
	require.NoError(t, err)

	_, err = session.Files().GetFileInfo(ctx, filePath)
	assert.Error(t, err, "expected error after deleting file")
}

func TestIsolationRWFilesMove(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	ts := time.Now().UnixMilli()
	srcPath := fmt.Sprintf("/tmp/test_move_src_%d.txt", ts)
	dstPath := fmt.Sprintf("/tmp/test_move_dst_%d.txt", ts)
	content := "move-me"

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte(content)),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: srcPath}},
	}})
	require.NoError(t, err)

	err = session.Files().MoveFiles(ctx, opensandbox.MoveRequest{{Src: srcPath, Dest: dstPath}})
	require.NoError(t, err)

	rc, err := session.Files().DownloadFile(ctx, dstPath, "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	_, err = session.Files().GetFileInfo(ctx, srcPath)
	assert.Error(t, err, "source file should no longer exist after move")
}

func TestIsolationRWFilesChmod(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_chmod_%d.txt", time.Now().UnixMilli())

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("chmod-me")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	err = session.Files().SetPermissions(ctx, opensandbox.PermissionsRequest{
		filePath: {Mode: 755},
	})
	require.NoError(t, err)

	info, err := session.Files().GetFileInfo(ctx, filePath)
	require.NoError(t, err)
	require.Contains(t, info, filePath)
	assert.Equal(t, 755, info[filePath].Mode)
}

func TestIsolationRWFilesReplace(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_replace_%d.txt", time.Now().UnixMilli())

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("old-content-here")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	err = session.Files().ReplaceInFiles(ctx, opensandbox.ReplaceRequest{
		filePath: {Old: "old-content", New: "new-content"},
	})
	require.NoError(t, err)

	rc, err := session.Files().DownloadFile(ctx, filePath, "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "new-content-here", string(data))
}

func TestIsolationRWFilesListDirectory(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	ts := time.Now().UnixMilli()
	dirPath := fmt.Sprintf("/tmp/test_listdir_%d", ts)
	err = session.Files().CreateDirectory(ctx, dirPath, 755)
	require.NoError(t, err)

	filePath := fmt.Sprintf("%s/child_%d.txt", dirPath, ts)
	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("list-me")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	entries, err := session.Files().ListDirectory(ctx, dirPath)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	found := false
	for _, fi := range entries {
		if strings.Contains(fi.Path, fmt.Sprintf("child_%d", ts)) {
			found = true
			break
		}
	}
	assert.True(t, found, "expected child file in directory listing")
}

func TestIsolationRWHostVisible(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	ts := time.Now().UnixMilli()
	filePath := fmt.Sprintf("/tmp/test_host_visible_%d.txt", ts)
	content := "visible-on-host"

	_, err = session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("echo -n %s > %s", content, filePath),
	}, nil)
	require.NoError(t, err)

	hostExec, err := sb.RunCommand(ctx, fmt.Sprintf("cat %s", filePath), nil)
	require.NoError(t, err)
	assert.Contains(t, hostExec.Text(), content)
}

// ---------------------------------------------------------------------------
// RO mode tests
// ---------------------------------------------------------------------------

func TestIsolationROCanReadExistingFiles(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	ts := time.Now().UnixMilli()
	filePath := fmt.Sprintf("/tmp/test_ro_read_%d.txt", ts)
	content := "host-created-content"

	_, err := sb.RunCommand(ctx, fmt.Sprintf("echo -n %s > %s", content, filePath), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "ro"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("cat %s", filePath),
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), content)
}

func TestIsolationROCannotWrite(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "ro"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_ro_write_%d.txt", time.Now().UnixMilli())

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("echo test > %s 2>&1; echo exit=$?", filePath),
	}, nil)
	require.NoError(t, err)
	text := exec.Text()
	assert.True(t,
		strings.Contains(text, "Read-only") ||
			strings.Contains(text, "read-only") ||
			strings.Contains(text, "Permission denied") ||
			strings.Contains(text, "exit=1"),
		"expected write to fail in RO mode, got: %s", text)
}

func TestIsolationROFilesAPIRead(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	ts := time.Now().UnixMilli()
	filePath := fmt.Sprintf("/tmp/test_ro_api_read_%d.txt", ts)
	content := "ro-api-read-content"

	_, err := sb.RunCommand(ctx, fmt.Sprintf("echo -n %s > %s", content, filePath), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "ro"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	rc, err := session.Files().DownloadFile(ctx, filePath, "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

func TestIsolationROFilesAPISearch(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	ts := time.Now().UnixMilli()
	filePath := fmt.Sprintf("/tmp/test_ro_search_%d.txt", ts)

	_, err := sb.RunCommand(ctx, fmt.Sprintf("echo -n data > %s", filePath), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "ro"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	results, err := session.Files().SearchFiles(ctx, "/tmp", fmt.Sprintf("test_ro_search_%d*", ts))
	require.NoError(t, err)
	require.NotEmpty(t, results)

	found := false
	for _, fi := range results {
		if strings.Contains(fi.Path, fmt.Sprintf("test_ro_search_%d", ts)) {
			found = true
			break
		}
	}
	assert.True(t, found, "expected to find file via search in RO session")
}

func TestIsolationROFilesAPIListDirectory(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	ts := time.Now().UnixMilli()
	dirPath := fmt.Sprintf("/tmp/test_ro_listdir_%d", ts)

	_, err := sb.RunCommand(ctx, fmt.Sprintf("mkdir -p %s && echo -n data > %s/child.txt", dirPath, dirPath), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "ro"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	entries, err := session.Files().ListDirectory(ctx, dirPath)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	found := false
	for _, fi := range entries {
		if strings.Contains(fi.Path, "child.txt") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected child.txt in RO directory listing")
}

// ---------------------------------------------------------------------------
// Overlay mode tests
// ---------------------------------------------------------------------------

func skipIfOverlayNotSupported(t *testing.T, ctx context.Context, sb *opensandbox.Sandbox) {
	t.Helper()
	caps, err := sb.IsolationCapabilities(ctx)
	require.NoError(t, err)
	if !caps.CommitSupported && !caps.DiffSupported {
		t.Skip("overlay mode not available")
	}
}

func TestIsolationOverlayWritesNotVisibleOnHost(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	ts := time.Now().UnixMilli()
	filePath := fmt.Sprintf("/tmp/test_overlay_invisible_%d.txt", ts)

	_, err = session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("echo -n overlay-data > %s", filePath),
	}, nil)
	require.NoError(t, err)

	hostExec, err := sb.RunCommand(ctx, fmt.Sprintf("cat %s 2>&1 || echo NOT_FOUND", filePath), nil)
	require.NoError(t, err)
	text := hostExec.Text()
	assert.True(t, strings.Contains(text, "NOT_FOUND") || strings.Contains(text, "No such file"),
		"overlay write should not be visible on host, got: %s", text)
}

func TestIsolationOverlayCanReadHostFiles(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	ts := time.Now().UnixMilli()
	filePath := fmt.Sprintf("/tmp/test_overlay_hostread_%d.txt", ts)
	content := "host-file-for-overlay"

	_, err := sb.RunCommand(ctx, fmt.Sprintf("echo -n %s > %s", content, filePath), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("cat %s", filePath),
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), content)
}

func TestIsolationOverlayCOWDoesNotMutateHost(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	ts := time.Now().UnixMilli()
	filePath := fmt.Sprintf("/tmp/test_overlay_cow_%d.txt", ts)
	originalContent := "original-content"

	_, err := sb.RunCommand(ctx, fmt.Sprintf("echo -n %s > %s", originalContent, filePath), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	_, err = session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("echo -n modified-content > %s", filePath),
	}, nil)
	require.NoError(t, err)

	// Verify session sees modified content
	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("cat %s", filePath),
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), "modified-content")

	// Verify host still has original content
	hostExec, err := sb.RunCommand(ctx, fmt.Sprintf("cat %s", filePath), nil)
	require.NoError(t, err)
	assert.Contains(t, hostExec.Text(), originalContent)
}

func TestIsolationOverlayFilesAPIUploadDownload(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_overlay_upload_%d.txt", time.Now().UnixMilli())
	content := "overlay-upload-content"

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte(content)),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	rc, err := session.Files().DownloadFile(ctx, filePath, "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	// Verify not visible on host
	hostExec, err := sb.RunCommand(ctx, fmt.Sprintf("cat %s 2>&1 || echo NOT_FOUND", filePath), nil)
	require.NoError(t, err)
	hostText := hostExec.Text()
	assert.True(t, strings.Contains(hostText, "NOT_FOUND") || strings.Contains(hostText, "No such file"),
		"overlay upload should not be visible on host, got: %s", hostText)
}

func TestIsolationOverlayFilesAPISearch(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	suffix := fmt.Sprintf("%d", time.Now().UnixMilli())
	filePath := fmt.Sprintf("/tmp/test_overlay_search_%s.txt", suffix)

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("overlay-search")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	results, err := session.Files().SearchFiles(ctx, "/tmp", fmt.Sprintf("test_overlay_search_%s*", suffix))
	require.NoError(t, err)
	require.NotEmpty(t, results)

	found := false
	for _, fi := range results {
		if strings.Contains(fi.Path, "test_overlay_search_"+suffix) {
			found = true
			break
		}
	}
	assert.True(t, found, "expected to find uploaded file in overlay search results")
}

func TestIsolationOverlayFilesAPIDelete(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_overlay_delete_%d.txt", time.Now().UnixMilli())

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("overlay-delete-me")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	err = session.Files().DeleteFiles(ctx, []string{filePath})
	require.NoError(t, err)

	_, err = session.Files().GetFileInfo(ctx, filePath)
	assert.Error(t, err, "expected error after deleting file in overlay session")
}

func TestIsolationOverlayFilesAPIMove(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	ts := time.Now().UnixMilli()
	srcPath := fmt.Sprintf("/tmp/test_overlay_move_src_%d.txt", ts)
	dstPath := fmt.Sprintf("/tmp/test_overlay_move_dst_%d.txt", ts)
	content := "overlay-move-me"

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte(content)),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: srcPath}},
	}})
	require.NoError(t, err)

	err = session.Files().MoveFiles(ctx, opensandbox.MoveRequest{{Src: srcPath, Dest: dstPath}})
	require.NoError(t, err)

	rc, err := session.Files().DownloadFile(ctx, dstPath, "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	_, err = session.Files().GetFileInfo(ctx, srcPath)
	assert.Error(t, err, "source file should no longer exist after move in overlay")
}

func TestIsolationOverlayFilesAPIChmod(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_overlay_chmod_%d.txt", time.Now().UnixMilli())

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("overlay-chmod")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	err = session.Files().SetPermissions(ctx, opensandbox.PermissionsRequest{
		filePath: {Mode: 755},
	})
	require.NoError(t, err)

	info, err := session.Files().GetFileInfo(ctx, filePath)
	require.NoError(t, err)
	require.Contains(t, info, filePath)
	assert.Equal(t, 755, info[filePath].Mode)
}

func TestIsolationOverlayFilesAPIReplace(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	filePath := fmt.Sprintf("/tmp/test_overlay_replace_%d.txt", time.Now().UnixMilli())

	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("old-overlay-text")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	err = session.Files().ReplaceInFiles(ctx, opensandbox.ReplaceRequest{
		filePath: {Old: "old-overlay", New: "new-overlay"},
	})
	require.NoError(t, err)

	rc, err := session.Files().DownloadFile(ctx, filePath, "")
	require.NoError(t, err)
	defer rc.Close()

	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "new-overlay-text", string(data))
}

func TestIsolationOverlayFilesAPIListDirectory(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)
	skipIfOverlayNotSupported(t, ctx, sb)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "overlay"},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	ts := time.Now().UnixMilli()
	dirPath := fmt.Sprintf("/tmp/test_overlay_listdir_%d", ts)

	err = session.Files().CreateDirectory(ctx, dirPath, 755)
	require.NoError(t, err)

	filePath := fmt.Sprintf("%s/overlay_child_%d.txt", dirPath, ts)
	err = session.Files().UploadFiles(ctx, []opensandbox.UploadFileEntry{{
		File:    bytes.NewReader([]byte("overlay-list-child")),
		Options: opensandbox.UploadFileOptions{Metadata: opensandbox.FileMetadata{Path: filePath}},
	}})
	require.NoError(t, err)

	entries, err := session.Files().ListDirectory(ctx, dirPath)
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	found := false
	for _, fi := range entries {
		if strings.Contains(fi.Path, fmt.Sprintf("overlay_child_%d", ts)) {
			found = true
			break
		}
	}
	assert.True(t, found, "expected child file in overlay directory listing")
}

// ---------------------------------------------------------------------------
// RunOnce / WithSession convenience API tests
// ---------------------------------------------------------------------------

func TestIsolationRunOnce(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	exec, err := sb.IsolationRunOnce(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	}, opensandbox.IsolatedRunRequest{Code: "echo run-once-e2e"}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), "run-once-e2e")
}

func TestIsolationRunOnceWithEnvs(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	exec, err := sb.IsolationRunOnce(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	}, opensandbox.IsolatedRunRequest{
		Code: "echo $E2E_VAR",
		Envs: map[string]string{"E2E_VAR": "run-once-val"},
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), "run-once-val")
}

// ---------------------------------------------------------------------------
// Bind mount tests (explicit source->dest binds)
// ---------------------------------------------------------------------------

// TestIsolationBindReadWriteHostVisible verifies a legal read-write bind:
// data written inside the bwrap namespace at the bind destination is readable
// on the host at the bind source.
func TestIsolationBindReadWriteHostVisible(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	ts := time.Now().UnixMilli()
	// Source must fall within the execd writable allowlist (e.g. /data).
	srcDir := fmt.Sprintf("/data/bind_rw_%d", ts)
	dest := "/mnt/bind_rw"
	fileName := "from_sandbox.txt"
	content := "bind-rw-visible-on-host"

	// Host-side: create the bind source directory and the destination mount
	// point (bwrap binds onto an existing dir; it cannot create one under the
	// read-only root).
	_, err := sb.RunCommand(ctx, fmt.Sprintf("mkdir -p %s %s", srcDir, dest), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
		Binds: []opensandbox.BindMount{
			{Source: srcDir, Dest: dest},
		},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	// Read back inside the namespace after writing (write + read in sandbox).
	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("echo -n %s > %s/%s && cat %s/%s", content, dest, fileName, dest, fileName),
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), content, "sandbox should read back what it wrote to the bind")

	// Host-side: the write must be visible at the bind source.
	hostExec, err := sb.RunCommand(ctx, fmt.Sprintf("cat %s/%s", srcDir, fileName), nil)
	require.NoError(t, err)
	assert.Contains(t, hostExec.Text(), content, "bind write should be visible on host")
}

// TestIsolationBindIllegalRejected verifies that a bind whose source is outside
// the writable allowlist is rejected at session creation.
func TestIsolationBindIllegalRejected(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	_, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
		Binds: []opensandbox.BindMount{
			// /etc is not in the writable allowlist.
			{Source: "/etc", Dest: "/mnt/etc"},
		},
	})
	require.Error(t, err, "bind with source outside allowlist should be rejected")
	assert.Contains(t, strings.ToLower(err.Error()), "allowlist",
		"error should indicate the allowlist rejection, got: %v", err)
}

// TestIsolationBindReadOnlyReadable verifies a read-only bind: a host-created
// file is readable inside the bwrap namespace via the read-only bind.
func TestIsolationBindReadOnlyReadable(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	ts := time.Now().UnixMilli()
	srcDir := fmt.Sprintf("/data/bind_ro_%d", ts)
	dest := "/mnt/bind_ro"
	fileName := "host_created.txt"
	content := "bind-ro-host-content"

	// Host-side: create the source dir (with a file to read), plus the
	// destination mount point that bwrap will bind onto.
	_, err := sb.RunCommand(ctx,
		fmt.Sprintf("mkdir -p %s %s && echo -n %s > %s/%s", srcDir, dest, content, srcDir, fileName), nil)
	require.NoError(t, err)

	session, err := sb.IsolationCreate(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
		Binds: []opensandbox.BindMount{
			{Source: srcDir, Dest: dest, ReadOnly: true},
		},
	})
	require.NoError(t, err)
	defer session.Delete(ctx)

	// Read the host-created file inside the namespace via the read-only bind.
	exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("cat %s/%s", dest, fileName),
	}, nil)
	require.NoError(t, err)
	assert.Contains(t, exec.Text(), content, "read-only bind should be readable inside the sandbox")

	// Writing through the read-only bind must fail.
	writeExec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{
		Code: fmt.Sprintf("echo x > %s/newfile.txt 2>&1; echo exit=$?", dest),
	}, nil)
	require.NoError(t, err)
	text := writeExec.Text()
	assert.True(t,
		strings.Contains(text, "Read-only") ||
			strings.Contains(text, "read-only") ||
			strings.Contains(text, "Permission denied") ||
			strings.Contains(text, "exit=1"),
		"expected write to fail through read-only bind, got: %s", text)
}

func TestIsolationWithSessionE2E(t *testing.T) {
	ctx, sb := createIsolatedTestSandbox(t)

	var output string
	err := sb.IsolationWithSession(ctx, opensandbox.CreateIsolatedSessionRequest{
		Workspace: opensandbox.IsolatedWorkspaceSpec{Path: "/tmp", Mode: "rw"},
	}, func(session *opensandbox.IsolationSession) error {
		_, err := session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "export WS_VAR=with-session-val"}, nil)
		if err != nil {
			return err
		}
		exec, err := session.Run(ctx, opensandbox.IsolatedRunRequest{Code: "echo $WS_VAR"}, nil)
		if err != nil {
			return err
		}
		output = exec.Text()
		return nil
	})
	require.NoError(t, err)
	assert.Contains(t, output, "with-session-val")
}
