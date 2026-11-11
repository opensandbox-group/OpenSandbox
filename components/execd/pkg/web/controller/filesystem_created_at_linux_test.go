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

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func statWithBirthTime(t *testing.T, filePath string) unix.Statx_t {
	t.Helper()
	var stat unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, filePath, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME|unix.STATX_CTIME, &stat)
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		t.Skipf("statx birth time is unavailable: %v", err)
	}
	require.NoError(t, err)
	if stat.Mask&unix.STATX_BTIME == 0 {
		t.Skip("filesystem does not report STATX_BTIME; cannot verify creation time against birth time")
	}
	require.NotZero(t, stat.Mask&unix.STATX_CTIME, "ctime is needed as a regression control")
	return stat
}

func fileInfoFromHandler(t *testing.T, filePath string) model.FileInfo {
	t.Helper()
	ctrl, rec := newFilesystemController(t, http.MethodGet, "/files/info?path="+url.QueryEscape(filePath), nil)
	ctrl.GetFilesInfo()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]model.FileInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	info, ok := resp[filePath]
	require.True(t, ok, "response missing entry for %s", filePath)
	return info
}

func TestFilesystemControllerGetFilesInfoCreatedAtSurvivesChmod(t *testing.T) {
	target := filepath.Join(t.TempDir(), "created.txt")
	require.NoError(t, os.WriteFile(target, []byte("content"), 0o644))
	beforeStat := statWithBirthTime(t, target)
	before := fileInfoFromHandler(t, target)

	time.Sleep(20 * time.Millisecond)
	require.NoError(t, os.Chmod(target, 0o600))
	afterStat := statWithBirthTime(t, target)
	after := fileInfoFromHandler(t, target)

	require.NotEqual(t, beforeStat.Ctime, afterStat.Ctime, "chmod must change ctime to expose the old implementation")
	require.Equal(t, beforeStat.Btime, afterStat.Btime, "birth time must remain unchanged")
	require.True(t, before.ModifiedAt.Equal(after.ModifiedAt), "chmod must leave modification time unchanged")
	require.Equal(t, 600, after.Permission.Mode)
	birthTime := time.Unix(beforeStat.Btime.Sec, int64(beforeStat.Btime.Nsec))
	require.True(t, before.CreatedAt.Equal(birthTime), "created_at before chmod must equal statx birth time")
	require.True(t, after.CreatedAt.Equal(birthTime), "created_at after chmod must equal statx birth time")
	require.True(t, before.CreatedAt.Equal(after.CreatedAt), "chmod must not change created_at when birth time is available")
}

func TestFilesystemControllerGetFilesInfoCreatedAtUsesSymlinkBirthTime(t *testing.T) {
	dir := t.TempDir()
	linkPath := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink("missing-target", linkPath))
	beforeStat := statWithBirthTime(t, linkPath)

	// A dangling link makes following the target fail. Rename changes the link's
	// ctime so falling back to ctime would also fail the birth-time assertion.
	time.Sleep(20 * time.Millisecond)
	renamedPath := filepath.Join(dir, "renamed-link")
	require.NoError(t, os.Rename(linkPath, renamedPath))
	afterStat := statWithBirthTime(t, renamedPath)
	info := fileInfoFromHandler(t, renamedPath)

	require.NotEqual(t, beforeStat.Ctime, afterStat.Ctime, "rename must change ctime to expose the old implementation")
	require.Equal(t, beforeStat.Btime, afterStat.Btime)
	require.Equal(t, "symlink", info.Type)
	birthTime := time.Unix(beforeStat.Btime.Sec, int64(beforeStat.Btime.Nsec))
	require.True(t, info.CreatedAt.Equal(birthTime), "created_at must use the dangling link's birth time")
}
