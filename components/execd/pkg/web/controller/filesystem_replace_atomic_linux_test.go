//go:build linux

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

import (
	"encoding/json"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

// A replacement that fails after the destination has been opened must leave
// the old bytes of the file intact. RLIMIT_FSIZE makes the write fail after a
// partial write, the same observable failure as a nearly full sandbox disk.
func TestFilesystemControllerReplaceContentFailureKeepsTheReplacedFile(t *testing.T) {
	const fileSizeLimit = 32

	dir := t.TempDir()
	target := filepath.Join(dir, "notes.md")
	original := strings.Repeat("previous content\n", 8)
	require.NoError(t, os.WriteFile(target, []byte(original), 0o640))

	payload, err := json.Marshal(map[string]model.ReplaceFileContentItem{
		target: {
			Old: "previous",
			New: "replacement",
		},
	})
	require.NoError(t, err)

	var oldLimit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &oldLimit))
	signal.Ignore(syscall.SIGXFSZ)
	t.Cleanup(func() {
		signal.Reset(syscall.SIGXFSZ)
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &oldLimit); err != nil {
			t.Errorf("restore RLIMIT_FSIZE: %v", err)
		}
	})

	limited := oldLimit
	limited.Cur = fileSizeLimit
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limited))

	ctrl, rec := newFilesystemController(t, http.MethodPost, "/files/replace", payload)
	ctrl.ReplaceContent()

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
	requireNoUploadStaging(t, dir)
}
