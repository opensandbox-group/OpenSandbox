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

// TestAtomicPolicyFileSaveRawNonDurable exercises the replayable-candidate
// surface: identical namespace effect and three-state classification as
// SaveRaw (byte-exact publish, atomic replace, full temp cleanup), with the
// durability barriers left out — which is not directly observable in-process,
// so the assertions pin the observable contract the replay invariant relies on.
func TestAtomicPolicyFileSaveRawNonDurable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s-1.json")
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)

	first := []byte(`{"v":1,"incarnation":"rt-1","revision":1,"vault":{}}` + "\n")
	state, err := s.SaveRawNonDurable(first)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(first), string(got))
	require.Equal(t, 0o600, fileMode(t, path), "a new file is 0600")
	assertNoAtomicTemps(t, dir)

	// A second non-durable save replaces atomically like SaveRaw.
	second := []byte(`{"v":1,"incarnation":"rt-1","revision":2,"vault":{}}` + "\n")
	state, err = s.SaveRawNonDurable(second)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(second), string(got))
	assertNoAtomicTemps(t, dir)

	// Existing mode/ownership are preserved on replacement, like SaveRaw.
	require.NoError(t, os.Chmod(path, 0o640))
	third := []byte(`{"v":1,"incarnation":"rt-1","revision":3,"vault":{}}` + "\n")
	state, err = s.SaveRawNonDurable(third)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(third), string(got))
	require.Equal(t, 0o640, fileMode(t, path), "replacement copies the existing mode")
	assertNoAtomicTemps(t, dir)

	// The durable path still works after non-durable publishes.
	fourth := []byte(`{"v":1,"incarnation":"rt-1","revision":4,"vault":{}}` + "\n")
	state, err = s.SaveRaw(fourth)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(fourth), string(got))
	assertNoAtomicTemps(t, dir)
}
