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

package isolation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchRejectsBadGlobOnEmptyTree(t *testing.T) {
	mv := newTestMergedView(t, WorkspaceRW)
	_, err := mv.Search(".", "[")
	require.ErrorIs(t, err, filepath.ErrBadPattern)

	root := t.TempDir()
	confined := NewConfinedView(root, uint32(os.Getuid()), uint32(os.Getgid()))
	_, err = confined.Search(".", "[")
	require.ErrorIs(t, err, filepath.ErrBadPattern)

	bind := NewBindView(root, false, uint32(os.Getuid()), uint32(os.Getgid()))
	_, err = bind.Search(".", "[")
	require.ErrorIs(t, err, filepath.ErrBadPattern)
}
