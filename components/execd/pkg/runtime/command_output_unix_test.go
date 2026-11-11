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

package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestRemoveCommandOutputFilesSkipsFifo verifies a FIFO swap cannot stall the
// janitor: the open must not wait for a reader, the non-regular entry is not
// truncated, and the unlink still proceeds.
func TestRemoveCommandOutputFilesSkipsFifo(t *testing.T) {
	tempDir := t.TempDir()
	fifo := filepath.Join(tempDir, "cccccccccccccccccccccccccccccccc.output")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))

	done := make(chan struct{})
	go func() {
		removeCommandOutputFiles(fifo)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("removeCommandOutputFiles blocked on a FIFO entry")
	}
	require.NoFileExists(t, fifo)
}

// TestRemoveCommandOutputFilesKeepsHardLinkContent verifies a hard-linked
// entry is only unlinked: with Nlink > 1 the inode's content must stay
// reachable through the other name instead of being truncated through the
// shared inode.
func TestRemoveCommandOutputFilesKeepsHardLinkContent(t *testing.T) {
	tempDir := t.TempDir()
	kept := filepath.Join(tempDir, "kept")
	entry := filepath.Join(tempDir, "dddddddddddddddddddddddddddddddd.output")
	require.NoError(t, os.WriteFile(entry, []byte("shared-inode-content"), 0o600))
	require.NoError(t, os.Link(entry, kept))

	removeCommandOutputFiles(entry)

	require.NoFileExists(t, entry)
	content, err := os.ReadFile(kept)
	require.NoError(t, err)
	require.Equal(t, "shared-inode-content", string(content))
}
