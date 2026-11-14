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

// Boundary-condition samples for the file upload API. Each test pins one edge
// of the upload surface that several concurrent PRs converge on from different
// directions. Tests whose expected behavior is still only proposed by an open
// PR are skipped with the PR number and the exact expectation, so they can be
// unskipped the moment that behavior lands. They are the empirical base for a
// spec issue on the file API surface.

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

// TestUploadRejectsUndefinedEnvVarPath pins the boundary behind PR #2690: a
// path that references an environment variable which is not set must be
// rejected as a validation error, never silently expanded (which would land
// the file at a literal "${VAR}" path). The validation itself is already in
// main; the open PR remaps the same rejection from a 500 to a 400, which the
// status range assertion below survives.
func TestUploadRejectsUndefinedEnvVarPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.ToSlash(filepath.Join(dir, "${OSB_FILE_API_BOUNDARY_UNDEFINED_VAR}", "file.txt"))

	rec := doUpload(t, `{"path":"`+target+`","mode":644}`, []byte("payload"))

	require.GreaterOrEqual(t, rec.Code, http.StatusBadRequest, "an undefined env var in the path must be rejected")
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Contains(t, resp.Message, "undefined environment variables")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "a rejected upload must not create anything on disk")
}

// TestUploadReplacementThroughAPIKeepsDestinationSetuid pins the boundary the
// staging upload path has to uphold end to end (PRs #2445 and #2750 both touch
// it): replacing an existing file through the public route must not strip the
// setuid bit, because the rename swaps the inode and the bit would otherwise
// be lost for good.
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

// TestUploadRequestedModeOverridesDestinationMode pins the other side of the
// mode boundary discussed in PRs #2661 and #2445: when the metadata carries an
// explicit mode, that request wins even for a replacement, where the staging
// path first restores the destination's previous mode.
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

// TestUploadRequestedModeAppliesSpecialBits pins the boundary behind open PRs
// #2750 and #2445: a metadata mode such as 4755 carries setuid/setgid/sticky
// in its high octal digits, and those must reach the file. Main today parses
// the octal digits and casts the integer straight to os.FileMode, which drops
// them because Go encodes the special bits at 1<<20 and above, leaving 0755.
// Unskip when one of those PRs lands.
func TestUploadRequestedModeAppliesSpecialBits(t *testing.T) {
	t.Skip("boundary sample for open PRs #2750/#2445: special bits in an octal metadata mode are proposed, not yet merged")
	if runtime.GOOS == "windows" {
		t.Skip("Windows only models the read-only bit")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "privileged.txt")

	rec := doUpload(t, `{"path":"`+filepath.ToSlash(target)+`","mode":4755}`, []byte("payload"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	require.NotZero(t, info.Mode()&os.ModeSetuid, "requested setuid from the octal metadata mode must be applied")
}
