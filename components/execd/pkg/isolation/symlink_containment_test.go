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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for #2332 follow-ups: containment must hold even when the
// sandbox races the files API, and for paths the views generate themselves.

func containmentDirs(t *testing.T) (src, outside, victim string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	src = filepath.Join(base, "src")
	outside = filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	victim = filepath.Join(outside, "victim.txt")
	require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
	return src, outside, victim
}

// swapLoop flips src/d between a real directory and a symlink to outside
// until stop is closed, emulating sandbox code racing a files API request.
func swapLoop(t *testing.T, src, outside string, stop <-chan struct{}, wg *sync.WaitGroup) {
	t.Helper()
	d := filepath.Join(src, "d")
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.RemoveAll(d)
			_ = os.Mkdir(d, 0o755)
			_ = os.Remove(d)
			_ = os.Symlink(outside, d)
		}
	}()
}

func TestBindView_SymlinkSwapRaceStaysContained(t *testing.T) {
	src, outside, victim := containmentDirs(t)
	view := NewBindView(src, false, uint32(os.Getuid()), uint32(os.Getgid()))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	swapLoop(t, src, outside, stop, &wg)

	var leaked atomic.Bool
	for i := 0; i < 2000; i++ {
		_ = view.WriteFile("/d/pwn.txt", []byte("x"), 0o644)
		_ = view.Chmod("/d/victim.txt", 0o777)
		_ = view.Remove("/d/victim.txt")
		if data, err := view.ReadFile("/d/victim.txt"); err == nil && string(data) == "untouched" {
			leaked.Store(true)
		}
	}
	close(stop)
	wg.Wait()

	assert.False(t, leaked.Load(), "read escaped the bind source during a swap race")
	_, err := os.Stat(filepath.Join(outside, "pwn.txt"))
	assert.True(t, os.IsNotExist(err), "write escaped the bind source during a swap race")
	info, err := os.Stat(victim)
	require.NoError(t, err, "victim removed through a swapped symlink")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "victim chmodded through a swapped symlink")
}

func TestMergedView_SymlinkSwapRaceStaysContained(t *testing.T) {
	upper, outside, victim := containmentDirs(t)
	lower := t.TempDir()
	view := NewMergedView(lower, upper, WorkspaceOverlay, uint32(os.Getuid()), uint32(os.Getgid()))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	swapLoop(t, upper, outside, stop, &wg)

	for i := 0; i < 2000; i++ {
		_ = view.WriteFile("d/pwn.txt", []byte("x"), 0o644)
		_ = view.Chmod("d/victim.txt", 0o777)
		_ = view.RemoveAll("d/victim.txt")
	}
	close(stop)
	wg.Wait()

	_, err := os.Stat(filepath.Join(outside, "pwn.txt"))
	assert.True(t, os.IsNotExist(err), "write escaped upper during a swap race")
	info, err := os.Stat(victim)
	require.NoError(t, err, "victim removed through a swapped symlink")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "victim chmodded through a swapped symlink")
}

func TestMergedView_WhiteoutDoesNotFollowPlantedSymlink(t *testing.T) {
	upper, _, victim := containmentDirs(t)
	lower := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(lower, "foo"), []byte("lower"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(lower, "bar"), []byte("lower"), 0o644))
	view := NewMergedView(lower, upper, WorkspaceOverlay, uint32(os.Getuid()), uint32(os.Getgid()))

	// Sandbox code plants the whiteout markers as links to a host victim.
	require.NoError(t, os.Symlink(victim, filepath.Join(upper, ".wh.foo")))
	require.NoError(t, os.Symlink(victim, filepath.Join(upper, ".wh.bar")))

	assert.Error(t, view.Remove("foo"))
	assert.Error(t, view.Rename("bar", "baz"))

	got, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(got), "whiteout creation truncated a host file")
}

func TestMergedView_WhiteoutReusesExistingMarker(t *testing.T) {
	upper := t.TempDir()
	lower := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(lower, "foo"), []byte("lower"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(upper, ".wh.foo"), nil, 0o644))
	view := NewMergedView(lower, upper, WorkspaceOverlay, uint32(os.Getuid()), uint32(os.Getgid()))

	require.NoError(t, view.RemoveAll("foo"))
	_, err := view.Stat("foo")
	assert.True(t, os.IsNotExist(err))
}

func TestBindView_SearchDoesNotWalkThroughEscapingRoot(t *testing.T) {
	src, outside, _ := containmentDirs(t)
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "etc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "etc", "secret.txt"), []byte("s"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "own.txt"), []byte("o"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(src, "escape")))
	view := NewBindView(src, false, 0, 0)

	results, err := view.Search("/escape/etc", "*.txt")
	if err == nil {
		assert.Empty(t, results, "search walked a root outside the bind source")
	}
	results, err = view.Search("/escape", "*.txt")
	if err == nil {
		assert.Empty(t, results, "search walked a root outside the bind source")
	}

	results, err = view.Search("/", "*.txt")
	require.NoError(t, err)
	assert.Equal(t, []string{"own.txt"}, results, "search must not descend into escaping links")
}
