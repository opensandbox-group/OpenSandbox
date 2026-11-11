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

// Birth-time boundary samples for /files/info, extracted from open PR #2612.
// created_at is read in two steps: the handler captures os.FileInfo for the
// response, and getFileCreateTime re-reads the path with statx to pick up the
// birth time. Between those two steps the directory entry can be replaced, so
// the birth time only belongs to the reported entry if the inode still
// matches. PR #2612 adds that identity check; these tests pin both sides of
// the boundary.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestGetFileCreateTimeUsesBirthTimeWhenIdentityMatches pins the contract the
// identity check in PR #2612 is built around: when the statx probe still
// resolves to the same inode as the captured FileInfo, created_at must be the
// file's birth time. This holds in main today and must keep holding once the
// identity check lands.
func TestGetFileCreateTimeUsesBirthTimeWhenIdentityMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stable.txt")
	require.NoError(t, os.WriteFile(path, []byte("content"), 0o644))

	stat := statWithBirthTime(t, path)
	birthTime := time.Unix(stat.Btime.Sec, int64(stat.Btime.Nsec))

	info, err := os.Lstat(path)
	require.NoError(t, err)
	require.Equal(t, stat.Ino, info.Sys().(*syscall.Stat_t).Ino, "test setup: the Lstat must observe the same inode")

	got := getFileCreateTime(path, info)
	require.True(t, got.Equal(birthTime), "created_at must use the file's birth time while the inode still matches")
}

// TestGetFileCreateTimeFallsBackWhenInodeDiffers pins the open half of PR
// #2612: when the path now refers to a different inode than the captured
// FileInfo, the replacement file's birth time must not be reported for the
// original entry; the captured FileInfo's ctime is the fallback. Main today
// trusts the statx birth time unconditionally. Unskip when #2612 lands.
func TestGetFileCreateTimeFallsBackWhenInodeDiffers(t *testing.T) {
	t.Skip("boundary sample for open PR #2612: the inode identity check with a ctime fallback is proposed, not yet merged")
	dir := t.TempDir()
	lookupPath := filepath.Join(dir, "lookup.txt")
	require.NoError(t, os.WriteFile(lookupPath, []byte("replacement"), 0o644))
	lookupStat := statWithBirthTime(t, lookupPath)
	lookupBirthTime := time.Unix(lookupStat.Btime.Sec, int64(lookupStat.Btime.Nsec))

	infoPath := filepath.Join(dir, "original.txt")
	require.NoError(t, os.WriteFile(infoPath, []byte("original"), 0o644))
	var info os.FileInfo
	var fallback time.Time
	// Establish distinct timestamps even on a filesystem with coarse clock
	// resolution. The inode mismatch itself is deterministic: both files stay
	// in place.
	require.Eventually(t, func() bool {
		if err := os.Chmod(infoPath, 0o600); err != nil {
			return false
		}
		var err error
		info, err = os.Lstat(infoPath)
		if err != nil {
			return false
		}
		stat := info.Sys().(*syscall.Stat_t)
		fallback = time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
		return !fallback.Equal(lookupBirthTime)
	}, 3*time.Second, time.Millisecond)
	require.NotEqual(t, lookupStat.Ino, info.Sys().(*syscall.Stat_t).Ino)

	got := getFileCreateTime(lookupPath, info)
	require.True(t, got.Equal(fallback), "created_at must fall back to the supplied inode's ctime")
	require.False(t, got.Equal(lookupBirthTime), "created_at must not use another inode's birth time")
}
