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

// Upload mode boundary tests that depend on the process umask. The umask
// decides what a create-mode becomes on disk, so these tests pin it
// explicitly instead of inheriting the CI environment's. They enforce the
// umask-dependent claims of the execd-file-api conformance profile
// (specs/conformance/profiles/execd-file-api.yaml).

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// TestUploadOmittedModeForNewFileFollowsCreatDefault probes the open side of
// claim FILE-UPLOAD-MODE-004 (PR #2661), which proposes that a new file
// uploaded without a metadata mode is created with creat(2)'s 0666 default
// shaped by the process umask — 0644 under umask 022 and 0600 under umask
// 077 — instead of remaining executable. Main today keeps the staging file at
// os.ModePerm masked by the umask, i.e. 0755 and 0700 for those umasks. The
// probe checks both umasks so a hard-coded 0644 implementation cannot satisfy
// the claim; it skips while every observation is still main's. Once the
// behavior lands it passes, and the conformance checker fails the stale
// pending claim so the profile gets promoted. Promotion replaces the skip
// below with the require.Equal assertions on both wanted modes, so the
// enforced form covers the umask shape, not just the 022 example.
func TestUploadOmittedModeForNewFileFollowsCreatDefault(t *testing.T) {
	cases := []struct {
		umask int
		want  os.FileMode
		main  os.FileMode
	}{
		{umask: 0o022, want: 0o644, main: 0o755},
		{umask: 0o077, want: 0o600, main: 0o700},
	}
	var pending []string
	for _, tc := range cases {
		withPinnedUmask(t, tc.umask)
		dir := t.TempDir()
		target := filepath.Join(dir, "defaulted.txt")

		rec := doUpload(t, `{"path":"`+filepath.ToSlash(target)+`"}`, []byte("payload"))

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		info, err := os.Stat(target)
		require.NoError(t, err)
		requireNoUploadStaging(t, dir)
		if info.Mode().Perm() == tc.want {
			continue
		}
		require.Equal(t, tc.main, info.Mode().Perm(), "unexpected mode while claim FILE-UPLOAD-MODE-004 is pending")
		pending = append(pending, fmt.Sprintf("umask %04o: got %04o, want %04o", tc.umask, info.Mode().Perm(), tc.want))
	}
	if len(pending) == 0 {
		return
	}
	t.Skipf("claim FILE-UPLOAD-MODE-004 is pending on PR #2661: %s", strings.Join(pending, "; "))
}
