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

package controller

// Upload mode boundary samples that depend on the process umask, extracted
// from open PR #2661. The umask decides what a create-mode becomes on disk,
// so these tests pin it explicitly instead of inheriting the CI
// environment's.

import (
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// withPinnedUmask replaces the process umask for the duration of the test.
// (PR #2661 ships the same helper under the name withUmask; the distinct name
// keeps this sample co-installable until one of the two lands.)
func withPinnedUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

// TestUploadOmittedModeForNewFileFollowsCreatDefault pins the boundary behind
// open PR #2661, which proposes that a new file uploaded without a metadata
// mode is created 0644 (creat(2)'s 0666 shaped by the umask) instead of
// remaining executable. Main today keeps the staging file at os.ModePerm
// masked by the umask, i.e. 0755 under umask 022. Unskip when #2661 lands.
func TestUploadOmittedModeForNewFileFollowsCreatDefault(t *testing.T) {
	t.Skip("boundary sample for open PR #2661: the 0644 default for a new file uploaded without a mode is proposed, not yet merged")
	withPinnedUmask(t, 0o022)
	dir := t.TempDir()
	target := filepath.Join(dir, "defaulted.txt")

	rec := doUpload(t, `{"path":"`+filepath.ToSlash(target)+`"}`, []byte("payload"))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "a new file uploaded without a mode must follow creat(2)'s 0644 default")
	requireNoUploadStaging(t, dir)
}
