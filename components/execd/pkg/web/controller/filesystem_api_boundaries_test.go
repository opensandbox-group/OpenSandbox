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

// Boundary-condition tests for the file upload API. Each test enforces one
// claim of the execd-file-api conformance profile
// (specs/conformance/profiles/execd-file-api.yaml, format proposed in
// issue #2926): the claim is the reviewable normative statement, the test is
// its executable form, and the checker in scripts/conformance keeps the two
// in sync. Tests whose expected behavior is still only proposed by an open PR
// are skipped with the PR number and the exact expectation, so they can be
// unskipped the moment that behavior lands.

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

// uploadBody builds an /files/upload request body with one metadata/file pair.
func uploadBody(t *testing.T, metadata string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	metadataPart, err := writer.CreateFormFile("metadata", "metadata")
	require.NoError(t, err)
	_, err = metadataPart.Write([]byte(metadata))
	require.NoError(t, err)
	filePart, err := writer.CreateFormFile("file", "payload.bin")
	require.NoError(t, err)
	_, err = filePart.Write(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return body, writer.FormDataContentType()
}

func doUpload(t *testing.T, metadata string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := uploadBody(t, metadata, content)
	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/upload", body.Bytes())
	ctrl.ctx.Request.Header.Set("Content-Type", contentType)
	ctrl.UploadFile()
	return rec
}

// TestUploadRejectsUndefinedEnvVarPath enforces claim FILE-PATH-ENV-001: a
// path that references an environment variable which is not set must be
// rejected as an error, never silently expanded (which would land the file at
// a literal "${VAR}" path). The status-range assertion survives the route's
// error mapping moving between 4xx and 5xx while the rejection itself stays.
func TestUploadRejectsUndefinedEnvVarPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.ToSlash(filepath.Join(dir, "${OSB_FILE_API_BOUNDARY_UNDEFINED_VAR}", "file.txt"))

	rec := doUpload(t, `{"path":"`+target+`","mode":644}`, []byte("payload"))

	require.GreaterOrEqual(t, rec.Code, http.StatusBadRequest, "an undefined env var in the path must be rejected")
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Contains(t, resp.Message, "undefined environment variables")
	require.Contains(t, resp.Message, "OSB_FILE_API_BOUNDARY_UNDEFINED_VAR", "the error must name the undefined variable")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "a rejected upload must not create anything on disk")
}

// TestUploadReplacementThroughAPIKeepsDestinationSetuid enforces claim
// FILE-UPLOAD-SETUID-002: the staging upload path must keep the destination's
// special bits end to end. Replacing an existing file through the public route
// must not strip the setuid bit, because the rename swaps the inode and the
// bit would otherwise be lost for good.
func TestUploadReplacementThroughAPIKeepsDestinationSetuid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows only models the read-only bit")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "helper")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o750))
	require.NoError(t, os.Chmod(target, 0o750|os.ModeSetuid))
	before, err := os.Stat(target)
	require.NoError(t, err)
	if before.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem does not keep setuid bits")
	}

	rec := doUpload(t, `{"path":"`+filepath.ToSlash(target)+`"}`, []byte("new"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750), info.Mode().Perm())
	require.NotZero(t, info.Mode()&os.ModeSetuid, "an API upload must not strip the destination's setuid bit")
	requireNoUploadStaging(t, dir)
}

// TestUploadRequestedModeOverridesDestinationMode enforces claim
// FILE-UPLOAD-MODE-003: when the metadata carries an explicit mode, that
// request wins even for a replacement, where the staging path first restores
// the destination's previous mode.
func TestUploadRequestedModeOverridesDestinationMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows only models the read-only bit")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "guarded.txt")
	require.NoError(t, os.WriteFile(target, []byte("old"), 0o600))

	rec := doUpload(t, `{"path":"`+filepath.ToSlash(target)+`","mode":644}`, []byte("new"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "an explicit metadata mode must win over the destination's mode")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new", string(data))
	requireNoUploadStaging(t, dir)
}

// TestUploadRequestedModeAppliesSpecialBits probes the open side of claim
// FILE-UPLOAD-MODE-005 (PR #2445): a metadata mode such as 4755 carries
// setuid/setgid/sticky in its high octal digits, and those must reach the
// file. Main today parses the octal digits and casts the integer straight to
// os.FileMode, which drops them because Go encodes the special bits at 1<<20
// and above, leaving 0755. The test skips while the observed mode is still
// main's; once the behavior lands it passes, and the conformance checker fails
// the stale pending claim so the profile gets promoted.
func TestUploadRequestedModeAppliesSpecialBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows only models the read-only bit")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "privileged.txt")

	rec := doUpload(t, `{"path":"`+filepath.ToSlash(target)+`","mode":4755}`, []byte("payload"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "unexpected permission bits while claim FILE-UPLOAD-MODE-005 is pending")
	if info.Mode()&os.ModeSetuid != 0 {
		return
	}
	t.Skip("claim FILE-UPLOAD-MODE-005 is pending on PR #2445: the requested setuid bit is not applied yet")
}
