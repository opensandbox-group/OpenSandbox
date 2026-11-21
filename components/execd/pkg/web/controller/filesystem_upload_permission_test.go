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

//go:build !windows

package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	unknownUploadOwner = "no_such_user_xyz"
	unknownUploadGroup = "no_such_group_xyz"

	uploadSpecialBits = os.ModeSetuid | os.ModeSetgid | os.ModeSticky
)

// postUploadMetadata sends one metadata/file pair through UploadFile. The
// metadata is marshalled as given, so a key left out of the map is omitted from
// the request and a key set to 0 is sent as an explicit 0.
func postUploadMetadata(t *testing.T, metadata map[string]any, content string) *httptest.ResponseRecorder {
	t.Helper()
	metadataJSON, err := json.Marshal(metadata)
	require.NoError(t, err)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("metadata", "metadata.json")
	require.NoError(t, err)
	_, err = part.Write(metadataJSON)
	require.NoError(t, err)
	part, err = writer.CreateFormFile("file", "payload.bin")
	require.NoError(t, err)
	_, err = part.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/upload", body.Bytes())
	ctrl.ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	ctrl.UploadFile()
	return rec
}

// uploadMetadataFor returns metadata for path with the extra keys added.
func uploadMetadataFor(path string, extra map[string]any) map[string]any {
	metadata := map[string]any{"path": path}
	for k, v := range extra {
		metadata[k] = v
	}
	return metadata
}

// assertInvalidUploadMetadata checks for the structured 400 error. It does not
// stop the test, so the checks that nothing was written still run after it.
func assertInvalidUploadMetadata(t *testing.T, rec *httptest.ResponseRecorder, wantInMessage string) {
	t.Helper()
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, model.ErrorCodeInvalidFileMetadata, resp.Code)
	assert.Contains(t, resp.Message, wantInMessage)
}

// uploadTargetState is what a rejected upload must leave untouched.
type uploadTargetState struct {
	content     string
	mode        os.FileMode
	modTimeNano int64
	uid         uint32
	gid         uint32
}

func readUploadTargetState(t *testing.T, path string) uploadTargetState {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return uploadTargetState{
		content:     string(content),
		mode:        info.Mode(),
		modTimeNano: info.ModTime().UnixNano(),
		uid:         stat.Uid,
		gid:         stat.Gid,
	}
}

// newExistingUploadTarget creates a destination whose content, mode and
// modification time are easy to tell apart from anything an upload would write.
func newExistingUploadTarget(t *testing.T, dir string) string {
	t.Helper()
	target := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(target, []byte("old content"), 0o640))
	require.NoError(t, os.Chmod(target, 0o640))
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(target, past, past))
	return target
}

// requireSpecialBitSupport skips the test where the platform or filesystem does
// not accept or keep setuid, setgid and sticky bits on a regular file.
func requireSpecialBitSupport(t *testing.T) {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "probe")
	require.NoError(t, os.WriteFile(probe, nil, 0o600))
	if err := os.Chmod(probe, 0o755|uploadSpecialBits); err != nil {
		t.Skipf("setuid, setgid and sticky bits are not accepted here: %v", err)
	}
	info, err := os.Stat(probe)
	require.NoError(t, err)
	if info.Mode()&uploadSpecialBits != uploadSpecialBits {
		t.Skip("filesystem does not keep setuid, setgid and sticky bits")
	}
}

type uploadAccount struct {
	owner, group string
	uid, gid     uint32
}

func lookupUploadAccount(owner, group string) (uploadAccount, error) {
	userInfo, err := user.Lookup(owner)
	if err != nil {
		return uploadAccount{}, err
	}
	groupInfo, err := user.LookupGroup(group)
	if err != nil {
		return uploadAccount{}, err
	}
	uid, err := strconv.ParseUint(userInfo.Uid, 10, 32)
	if err != nil {
		return uploadAccount{}, err
	}
	gid, err := strconv.ParseUint(groupInfo.Gid, 10, 32)
	if err != nil {
		return uploadAccount{}, err
	}
	return uploadAccount{owner: owner, group: group, uid: uint32(uid), gid: uint32(gid)}, nil
}

// currentUploadAccount is the account of the test process, so ownership can be
// applied without privileges.
func currentUploadAccount(t *testing.T) uploadAccount {
	t.Helper()
	current, err := user.Current()
	require.NoError(t, err)
	group, err := user.LookupGroupId(current.Gid)
	if err != nil {
		t.Skipf("current group has no entry: %v", err)
	}
	account, err := lookupUploadAccount(current.Username, group.Name)
	if err != nil {
		t.Skipf("current account has no entry: %v", err)
	}
	return account
}

// otherUploadAccount is an existing account other than the caller's, for the
// cases that need root to chown to it.
func otherUploadAccount(t *testing.T) uploadAccount {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("changing the owner to another account requires root")
	}
	for _, candidate := range [][2]string{{"nobody", "nogroup"}, {"nobody", "nobody"}, {"daemon", "daemon"}} {
		if account, err := lookupUploadAccount(candidate[0], candidate[1]); err == nil {
			return account
		}
	}
	t.Skip("no account to chown to")
	return uploadAccount{}
}

func requireOwnedBy(t *testing.T, account uploadAccount, paths ...string) {
	t.Helper()
	for _, path := range paths {
		info, err := os.Stat(path)
		require.NoError(t, err)
		stat, ok := info.Sys().(*syscall.Stat_t)
		require.True(t, ok)
		require.Equal(t, account.uid, stat.Uid, path)
		require.Equal(t, account.gid, stat.Gid, path)
	}
}

// A request that names an owner or group the sandbox does not have is a client
// error. It must be rejected before a directory is created or the destination
// is opened, so nothing is written, truncated or chown'd.
func TestUploadFileRejectsUnknownOwnerOrGroupBeforeWriting(t *testing.T) {
	current := currentUploadAccount(t)
	cases := []struct {
		name     string
		metadata map[string]any
		want     string
	}{
		{"unknown owner", map[string]any{"owner": unknownUploadOwner}, unknownUploadOwner},
		{"unknown group", map[string]any{"group": unknownUploadGroup}, unknownUploadGroup},
		{"known owner and unknown group", map[string]any{"owner": current.owner, "group": unknownUploadGroup}, unknownUploadGroup},
		{"unknown owner with a valid mode", map[string]any{"owner": unknownUploadOwner, "mode": 600}, unknownUploadOwner},
	}

	for _, tt := range cases {
		t.Run(tt.name+" absent destination", func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "absent.txt")

			rec := postUploadMetadata(t, uploadMetadataFor(target, tt.metadata), "new content")

			assertInvalidUploadMetadata(t, rec, tt.want)
			_, err := os.Stat(target)
			require.ErrorIs(t, err, os.ErrNotExist, "a rejected upload must not create the file")
			requireNoUploadStaging(t, filepath.Dir(target))
		})

		t.Run(tt.name+" missing parent directories", func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "new", "nested", "absent.txt")

			rec := postUploadMetadata(t, uploadMetadataFor(target, tt.metadata), "new content")

			assertInvalidUploadMetadata(t, rec, tt.want)
			_, err := os.Stat(filepath.Join(dir, "new"))
			require.ErrorIs(t, err, os.ErrNotExist, "a rejected upload must not create parent directories")
		})

		t.Run(tt.name+" existing destination", func(t *testing.T) {
			target := newExistingUploadTarget(t, t.TempDir())
			before := readUploadTargetState(t, target)

			rec := postUploadMetadata(t, uploadMetadataFor(target, tt.metadata), "new content")

			assertInvalidUploadMetadata(t, rec, tt.want)
			require.Equal(t, before, readUploadTargetState(t, target), "a rejected upload must leave content and metadata as they were")
			requireNoUploadStaging(t, filepath.Dir(target))
		})
	}
}

// A mode that is not octal digits between 0 and 7777 cannot be applied, so it
// is rejected up front as well instead of after the content was written.
func TestUploadFileRejectsInvalidModeBeforeWriting(t *testing.T) {
	for _, mode := range []int{-1, 8, 9, 99, 7778, 10000, 100644} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			dir := t.TempDir()

			absent := filepath.Join(dir, "nested", "absent.txt")
			rec := postUploadMetadata(t, map[string]any{"path": absent, "mode": mode}, "new content")
			assertInvalidUploadMetadata(t, rec, strconv.Itoa(mode))
			_, err := os.Stat(filepath.Join(dir, "nested"))
			require.ErrorIs(t, err, os.ErrNotExist, "a rejected upload must not create the file or its directories")

			existing := newExistingUploadTarget(t, dir)
			before := readUploadTargetState(t, existing)
			rec = postUploadMetadata(t, map[string]any{"path": existing, "mode": mode}, "new content")
			assertInvalidUploadMetadata(t, rec, strconv.Itoa(mode))
			require.Equal(t, before, readUploadTargetState(t, existing), "a rejected upload must leave content and metadata as they were")
			requireNoUploadStaging(t, dir)
		})
	}
}

// The mode is octal digits in a decimal integer, as in the spec (755, 4755).
// An explicit 0 is a mode, and the setuid, setgid and sticky digits are applied
// instead of being dropped by a direct os.FileMode conversion.
func TestUploadFileAppliesExplicitMode(t *testing.T) {
	cases := []struct {
		mode int
		want os.FileMode
	}{
		{644, 0o644},
		{600, 0o600},
		{0, 0},
		{4755, 0o755 | os.ModeSetuid},
		{2755, 0o755 | os.ModeSetgid},
		{1777, 0o777 | os.ModeSticky},
		{6750, 0o750 | os.ModeSetuid | os.ModeSetgid},
		{7777, 0o777 | uploadSpecialBits},
	}

	for _, tt := range cases {
		t.Run(fmt.Sprintf("new file mode %d", tt.mode), func(t *testing.T) {
			if tt.want&uploadSpecialBits != 0 {
				requireSpecialBitSupport(t)
			}
			target := filepath.Join(t.TempDir(), "new.txt")

			rec := postUploadMetadata(t, map[string]any{"path": target, "mode": tt.mode}, "payload")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, tt.want, info.Mode())
		})

		t.Run(fmt.Sprintf("existing file mode %d", tt.mode), func(t *testing.T) {
			if tt.want&uploadSpecialBits != 0 {
				requireSpecialBitSupport(t)
			}
			target := newExistingUploadTarget(t, t.TempDir())

			rec := postUploadMetadata(t, map[string]any{"path": target, "mode": tt.mode}, "payload")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, tt.want, info.Mode())
		})
	}
}

// Without a mode (omitted or null) the upload behaves as before: a new file
// gets the create default and an existing file keeps its mode.
func TestUploadFileOmittedModeKeepsDefaults(t *testing.T) {
	reference := filepath.Join(t.TempDir(), "reference")
	f, err := os.OpenFile(reference, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.ModePerm)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	referenceInfo, err := os.Stat(reference)
	require.NoError(t, err)

	cases := []struct {
		name  string
		extra map[string]any
	}{
		{"omitted", nil},
		{"null", map[string]any{"mode": nil}},
	}
	for _, tt := range cases {
		t.Run(tt.name+" new file", func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "new.txt")

			rec := postUploadMetadata(t, uploadMetadataFor(target, tt.extra), "payload")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			info, err := os.Stat(target)
			require.NoError(t, err)
			require.Equal(t, referenceInfo.Mode(), info.Mode())
		})

		t.Run(tt.name+" existing file", func(t *testing.T) {
			target := newExistingUploadTarget(t, t.TempDir())

			rec := postUploadMetadata(t, uploadMetadataFor(target, tt.extra), "payload")

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			state := readUploadTargetState(t, target)
			require.Equal(t, "payload", state.content)
			require.Equal(t, os.FileMode(0o640), state.mode)
		})
	}
}

// Valid ownership metadata is applied, including to the directories the upload
// creates, and a mode in the same request is applied on top of it.
func TestUploadFileAppliesOwnerAndGroup(t *testing.T) {
	current := currentUploadAccount(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "created", "nested", "owned.txt")

	rec := postUploadMetadata(t, map[string]any{"path": target, "owner": current.owner, "group": current.group, "mode": 640}, "payload")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireOwnedBy(t, current, filepath.Join(dir, "created"), filepath.Join(dir, "created", "nested"), target)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode())
}

// chown clears setuid and setgid on a regular file, so an upload that sets both
// ownership and a special bit has to change the owner first.
func TestUploadFileSpecialBitsSurviveOwnerChange(t *testing.T) {
	requireSpecialBitSupport(t)
	current := currentUploadAccount(t)
	target := filepath.Join(t.TempDir(), "helper")

	rec := postUploadMetadata(t, map[string]any{"path": target, "owner": current.owner, "group": current.group, "mode": 6750}, "payload")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750)|os.ModeSetuid|os.ModeSetgid, info.Mode())
}

// With privileges the owner and group really change to another account, and the
// requested special bit is still present afterwards.
func TestUploadFileChangesOwnerAndGroupWithSpecialBit(t *testing.T) {
	requireSpecialBitSupport(t)
	other := otherUploadAccount(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "created", "owned.txt")

	rec := postUploadMetadata(t, map[string]any{"path": target, "owner": other.owner, "group": other.group, "mode": 4750}, "payload")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireOwnedBy(t, other, filepath.Join(dir, "created"), target)
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o750)|os.ModeSetuid, info.Mode())
}

// Control: a multipart body that ends before its closing boundary is rejected
// while the form is parsed, before any file is touched.
func TestUploadFileAbortedMultipartKeepsExistingFile(t *testing.T) {
	target := newExistingUploadTarget(t, t.TempDir())
	before := readUploadTargetState(t, target)

	metadataJSON, err := json.Marshal(map[string]any{"path": target, "mode": 600})
	require.NoError(t, err)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("metadata", "metadata.json")
	require.NoError(t, err)
	_, err = part.Write(metadataJSON)
	require.NoError(t, err)
	part, err = writer.CreateFormFile("file", "payload.bin")
	require.NoError(t, err)
	_, err = part.Write(bytes.Repeat([]byte("N"), 1<<16))
	require.NoError(t, err)
	// The writer is deliberately not closed, and the body is cut inside the file part.
	truncated := body.Bytes()[:body.Len()-(1<<15)]

	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/upload", truncated)
	ctrl.ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	ctrl.UploadFile()

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Equal(t, before, readUploadTargetState(t, target))
	requireNoUploadStaging(t, filepath.Dir(target))
}
