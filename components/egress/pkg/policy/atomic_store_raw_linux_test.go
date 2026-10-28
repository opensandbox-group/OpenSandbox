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

package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAtomicPolicyFileSaveRawAndDelete exercises the raw-bytes and purge
// surface the vault persistence shares: byte-exact SaveRaw, idempotent
// absent-file Delete, full temp cleanup.
func TestAtomicPolicyFileSaveRawAndDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s-1.json")
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)

	// Absent-file delete is a complete no-op purge (FileUnchanged, nil).
	state, err := s.Delete()
	require.NoError(t, err)
	require.Equal(t, FileUnchanged, state)

	// SaveRaw publishes exact bytes (including the trailing newline) and
	// preserves no stale temp state.
	first := []byte(`{"v":1,"incarnation":"rt-1","revision":1,"vault":{}}` + "\n")
	state, err = s.SaveRaw(first)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(first), string(got))
	require.Equal(t, 0o600, fileMode(t, path), "a new file is 0600")
	assertNoAtomicTemps(t, dir)

	// A second SaveRaw replaces atomically; a snapshot of the previous image
	// restores it byte-exact.
	second := []byte(`{"v":1,"incarnation":"rt-1","revision":2,"vault":{}}` + "\n")
	snapshot, err := s.Snapshot()
	require.NoError(t, err)
	state, err = s.SaveRaw(second)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(second), string(got))
	state, err = s.Restore(snapshot)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(first), string(got))

	// Delete purges durably and is idempotent afterwards.
	state, err = s.Delete()
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	_, err = os.Lstat(path)
	require.True(t, os.IsNotExist(err))
	assertNoAtomicTemps(t, dir)
	state, err = s.Delete()
	require.NoError(t, err)
	require.Equal(t, FileUnchanged, state)
}

func fileMode(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return int(info.Mode().Perm())
}
