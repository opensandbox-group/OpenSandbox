// Copyright 2026 Alibaba Group Holding Ltd.
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

package mitmproxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCandidateCACertPaths(t *testing.T) {
	h := "/var/lib/mitmproxy"
	got := candidateCACertPaths("", h)
	require.Equal(t, []string{filepath.Join(h, ".mitmproxy", mitmCACertName)}, got)

	got = candidateCACertPaths("/custom", h)
	require.Equal(t, []string{
		filepath.Join("/custom", mitmCACertName),
		filepath.Join("/custom", ".mitmproxy", mitmCACertName),
		filepath.Join(h, ".mitmproxy", mitmCACertName),
	}, got)
}

func TestPurgeStaleExportedCAFrom_RemovesExistingFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, mitmCACertName)
	require.NoError(t, os.WriteFile(path, []byte("stale-ca"), 0o644))

	purgeStaleExportedCAFrom(root)

	_, err := os.Stat(path)
	require.True(t, os.IsNotExist(err), "expected file to be removed, got err=%v", err)
}

func TestPurgeStaleExportedCAFrom_NoFileIsNoOp(t *testing.T) {
	root := t.TempDir()
	// No file present; must not panic and must not create anything.
	purgeStaleExportedCAFrom(root)

	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestPurgeStaleExportedCAFrom_LeavesUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, mitmCACertName)
	require.NoError(t, os.WriteFile(stale, []byte("stale-ca"), 0o644))

	// A sibling file that must survive (e.g. the agent's merged bundle).
	other := filepath.Join(root, "merged-ca-certificates.pem")
	require.NoError(t, os.WriteFile(other, []byte("merged"), 0o644))

	purgeStaleExportedCAFrom(root)

	_, err := os.Stat(stale)
	require.True(t, os.IsNotExist(err))
	_, err = os.Stat(other)
	require.NoError(t, err)
}
