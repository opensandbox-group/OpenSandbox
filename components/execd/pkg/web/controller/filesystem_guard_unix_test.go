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
