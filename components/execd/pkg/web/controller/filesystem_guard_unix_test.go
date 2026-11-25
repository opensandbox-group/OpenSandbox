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
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestFilesystemControllerRejectsFIFO(t *testing.T) {
	withTestFilesystemGuard(t, FilesystemGuardPolicy{})

	fifoPath := filepath.Join(t.TempDir(), "hang.fifo")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0o600))

	ctrl, rec := newFilesystemController(t, http.MethodGet, "/files/download?path="+url.QueryEscape(fifoPath), nil)
	ctrl.DownloadFile()
	assertForbiddenResponse(t, rec)

	body, err := json.Marshal(map[string]model.ReplaceFileContentItem{
		fifoPath: {Old: "a", New: "b"},
	})
	require.NoError(t, err)
	ctrl2, rec2 := newFilesystemController(t, http.MethodPost, "/files/replace", body)
	ctrl2.ReplaceContent()
	assertForbiddenResponse(t, rec2)
}

func TestOpenGuardedFileSymlinkSwapTOCTOU(t *testing.T) {
	tmp := t.TempDir()
	allowedFile := filepath.Join(tmp, "allowed.txt")
	blockedDir := filepath.Join(tmp, "blocked")
	blockedFile := filepath.Join(blockedDir, "secret.txt")
	require.NoError(t, os.MkdirAll(blockedDir, 0o755))
	require.NoError(t, os.WriteFile(allowedFile, []byte("PUBLIC_DATA"), 0o644))
	require.NoError(t, os.WriteFile(blockedFile, []byte("SECRET_PAYLOAD"), 0o600))

	linkPath := filepath.Join(tmp, "race-link")
	require.NoError(t, os.Symlink(allowedFile, linkPath))

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{blockedDir},
	})

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		swapTmp := filepath.Join(tmp, "race-link.tmp")
		for i := 0; !stop.Load(); i++ {
			target := allowedFile
			if i%2 == 0 {
				target = blockedFile
			}
			_ = os.Remove(swapTmp)
			if err := os.Symlink(target, swapTmp); err == nil {
				_ = os.Rename(swapTmp, linkPath)
			}
		}
	}()

	for range 2000 {
		f, err := openGuardedReadFile(linkPath)
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(f)
		_ = f.Close()
		require.NoError(t, readErr)
		require.False(t, strings.Contains(string(data), "SECRET_PAYLOAD"), "TOCTOU leaked blocked regular file")
	}

	stop.Store(true)
	wg.Wait()
}

func TestGuardedMutationsParentSymlinkSwapTOCTOU(t *testing.T) {
	tmp := t.TempDir()
	blockedDir := filepath.Join(tmp, "blocked")
	require.NoError(t, os.MkdirAll(blockedDir, 0o755))
	secretFile := filepath.Join(blockedDir, "secret.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("SECRET"), 0o600))

	realDir := filepath.Join(tmp, "realdir")
	require.NoError(t, os.MkdirAll(realDir, 0o755))
	realFile := filepath.Join(realDir, "secret.txt")
	require.NoError(t, os.WriteFile(realFile, []byte("PUBLIC"), 0o600))

	slot := filepath.Join(tmp, "slot")
	require.NoError(t, os.Symlink(realDir, slot))

	withTestFilesystemGuard(t, FilesystemGuardPolicy{
		ExtraBlockedPaths: []string{blockedDir},
	})

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		swapTmp := filepath.Join(tmp, "slot.tmp")
		for i := 0; !stop.Load(); i++ {
			target := realDir
			if i%2 == 0 {
				target = blockedDir
			}
			_ = os.Remove(swapTmp)
			if err := os.Symlink(target, swapTmp); err == nil {
				_ = os.Rename(swapTmp, slot)
			}
		}
	}()

	targetPath := filepath.Join(slot, "secret.txt")
	exfilDest := filepath.Join(tmp, "exfil.txt")
	for range 1500 {
		_ = ChmodFile(targetPath, model.Permission{Mode: 777})
		_ = RenameFile(model.RenameFileItem{Src: targetPath, Dest: exfilDest})
		// Restore realFile if it was legitimately moved out of realDir.
		if _, err := os.Stat(exfilDest); err == nil {
			data, _ := os.ReadFile(exfilDest)
			require.NotEqual(t, "SECRET", string(data), "TOCTOU exfiltrated blocked file via RenameFile")
			_ = os.Rename(exfilDest, realFile)
		}
	}

	stop.Store(true)
	wg.Wait()

	info, err := os.Stat(secretFile)
	require.NoError(t, err, "blocked secret file must still exist")
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "TOCTOU chmodded blocked secret file")
}
