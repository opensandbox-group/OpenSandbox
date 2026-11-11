//go:build !linux

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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAtomicPolicyFileUnsupportedPlatform(t *testing.T) {
	store, err := NewAtomicPolicyFile("policy.json")
	require.Nil(t, store)
	require.ErrorContains(t, err, "requires Linux")
	store = &AtomicPolicyFile{}
	snapshot, err := store.Snapshot()
	require.Nil(t, snapshot)
	require.ErrorContains(t, err, "requires Linux")
	state, err := store.Save(DefaultDenyPolicy())
	require.Equal(t, FileUnchanged, state)
	require.ErrorContains(t, err, "requires Linux")
	state, err = store.Restore(nil)
	require.Equal(t, FileUnchanged, state)
	require.ErrorContains(t, err, "requires Linux")
}
