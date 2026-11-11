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
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Run these tests in a private mount namespace, as the egress CI workflow does:
//
//	go test -c -o /tmp/egress-policy.test ./pkg/policy
//	sudo env OPENSANDBOX_ATOMIC_POLICY_MOUNT_TEST=1 unshare --mount --propagation private \
//	  /tmp/egress-policy.test -test.run '^TestAtomicPolicy.*(Mount|Symlink)' -test.v
//
// Only an absent opt-in skips tests. Once enabled, missing mount privileges and
// unsupported mount operations are failures rather than false-positive passes.
func requireAtomicPolicyMountTests(t *testing.T) {
	t.Helper()
	switch os.Getenv("OPENSANDBOX_ATOMIC_POLICY_MOUNT_TEST") {
	case "":
		t.Skip("set OPENSANDBOX_ATOMIC_POLICY_MOUNT_TEST=1 in a private mount namespace")
	case "1":
		require.Equal(t, 0, os.Geteuid(), "mount integration tests require root in a private mount namespace")
	default:
		t.Fatal("OPENSANDBOX_ATOMIC_POLICY_MOUNT_TEST must be unset or 1")
	}
}

func TestAtomicPolicyDirectoryBindMount(t *testing.T) {
	requireAtomicPolicyMountTests(t)
	hostDir, mountedDir := atomicMountDirectories(t)
	hostPath := filepath.Join(hostDir, "policy.json")
	mountedPath := filepath.Join(mountedDir, "policy.json")
	// Whitespace is intentional: rollback must restore bytes, not reserialize JSON.
	original := []byte(" {\"egress\": [], \"defaultAction\": \"deny\"}\n\n")
	writeAtomicMountOriginal(t, hostPath, original)
	before := readAtomicMountFile(t, hostPath)
	oldFile, err := os.Open(hostPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, oldFile.Close()) })
	bindAtomicMount(t, hostDir, mountedDir, false)
	require.Equal(t, before, readAtomicMountFile(t, mountedPath))

	store, err := NewAtomicPolicyFile(mountedPath)
	require.NoError(t, err)
	snapshot, err := store.Snapshot()
	require.NoError(t, err)
	candidate, expected := atomicMountCandidate(t)
	state, err := store.Save(candidate)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)

	// Re-open the original host-visible path. A sidecar-only renamed copy would
	// leave this source path stale and fail these assertions.
	after := readAtomicMountFile(t, hostPath)
	require.Equal(t, expected, after.bytes)
	require.NotEqual(t, before.inode, after.inode, "save must replace the inode rather than truncate the live file")
	assertAtomicMountMetadata(t, before, after)
	require.Equal(t, after, readAtomicMountFile(t, mountedPath))
	oldBytes, err := io.ReadAll(oldFile)
	require.NoError(t, err)
	require.Equal(t, original, oldBytes, "an already-open reader must retain the complete original file")

	state, err = store.Restore(snapshot)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	restored := readAtomicMountFile(t, hostPath)
	require.Equal(t, original, restored.bytes)
	assertAtomicMountMetadata(t, before, restored)
	require.Equal(t, restored, readAtomicMountFile(t, mountedPath))
	assertAtomicMountEntries(t, hostDir, "policy.json")
}

func TestAtomicPolicyAbsentDirectoryBindMount(t *testing.T) {
	requireAtomicPolicyMountTests(t)
	hostDir, mountedDir := atomicMountDirectories(t)
	bindAtomicMount(t, hostDir, mountedDir, false)
	hostPath := filepath.Join(hostDir, "policy.json")
	mountedPath := filepath.Join(mountedDir, "policy.json")
	store, err := NewAtomicPolicyFile(mountedPath)
	require.NoError(t, err)
	snapshot, err := store.Snapshot()
	require.NoError(t, err)
	candidate, expected := atomicMountCandidate(t)

	state, err := store.Save(candidate)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	created := readAtomicMountFile(t, hostPath)
	require.Equal(t, expected, created.bytes)
	require.Equal(t, os.FileMode(0o600), created.mode)
	require.Equal(t, created, readAtomicMountFile(t, mountedPath))

	state, err = store.Restore(snapshot)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	for _, path := range []string{hostPath, mountedPath} {
		_, err := os.Lstat(path)
		require.True(t, os.IsNotExist(err), "restore must remove an initially absent policy: %s: %v", path, err)
	}
	assertAtomicMountEntries(t, hostDir)
}

func TestAtomicPolicySingleFileBindMountRejected(t *testing.T) {
	requireAtomicPolicyMountTests(t)
	hostDir, mountedDir := atomicMountDirectories(t)
	hostPath := filepath.Join(hostDir, "policy.json")
	mountedPath := filepath.Join(mountedDir, "policy.json")
	writeAtomicMountOriginal(t, hostPath, []byte("{\"defaultAction\":\"deny\",\"egress\":[]}\n"))
	require.NoError(t, os.WriteFile(mountedPath, nil, 0o600))
	bindAtomicMount(t, hostPath, mountedPath, false)
	before := readAtomicMountFile(t, hostPath)
	require.Equal(t, before, readAtomicMountFile(t, mountedPath))

	assertAtomicMountRejected(t, mountedPath)
	require.Equal(t, before, readAtomicMountFile(t, hostPath), "rejection must preserve host bytes and inode")
	require.Equal(t, before, readAtomicMountFile(t, mountedPath), "rejection must preserve bind-mounted bytes and inode")
	assertAtomicMountEntries(t, mountedDir, "policy.json")
}

func TestAtomicPolicyReadOnlyDirectoryBindMountRejected(t *testing.T) {
	requireAtomicPolicyMountTests(t)
	hostDir, mountedDir := atomicMountDirectories(t)
	hostPath := filepath.Join(hostDir, "policy.json")
	mountedPath := filepath.Join(mountedDir, "policy.json")
	writeAtomicMountOriginal(t, hostPath, []byte("{\"defaultAction\":\"deny\",\"egress\":[]}\n"))
	bindAtomicMount(t, hostDir, mountedDir, true)
	before := readAtomicMountFile(t, hostPath)
	require.Equal(t, before, readAtomicMountFile(t, mountedPath))

	assertAtomicMountRejected(t, mountedPath)
	require.Equal(t, before, readAtomicMountFile(t, hostPath))
	require.Equal(t, before, readAtomicMountFile(t, mountedPath))
	assertAtomicMountEntries(t, hostDir, "policy.json")
}

func TestAtomicPolicySymlinkRejected(t *testing.T) {
	requireAtomicPolicyMountTests(t)
	hostDir, mountedDir := atomicMountDirectories(t)
	hostTarget := filepath.Join(hostDir, "target.json")
	hostLink := filepath.Join(hostDir, "policy.json")
	mountedLink := filepath.Join(mountedDir, "policy.json")
	writeAtomicMountOriginal(t, hostTarget, []byte("{\"defaultAction\":\"deny\",\"egress\":[]}\n"))
	require.NoError(t, os.Symlink("target.json", hostLink))
	bindAtomicMount(t, hostDir, mountedDir, false)
	before := readAtomicMountFile(t, hostTarget)
	linkBefore, err := os.Lstat(hostLink)
	require.NoError(t, err)

	assertAtomicMountRejected(t, mountedLink)
	require.Equal(t, before, readAtomicMountFile(t, hostTarget))
	require.Equal(t, before, readAtomicMountFile(t, mountedLink))
	linkAfter, err := os.Lstat(hostLink)
	require.NoError(t, err)
	require.True(t, os.SameFile(linkBefore, linkAfter), "rejection must preserve the symlink inode")
	require.NotZero(t, linkAfter.Mode()&os.ModeSymlink)
	linkTarget, err := os.Readlink(hostLink)
	require.NoError(t, err)
	require.Equal(t, "target.json", linkTarget)
	assertAtomicMountEntries(t, hostDir, "policy.json", "target.json")
}

func atomicMountDirectories(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	hostDir, mountedDir := filepath.Join(root, "host"), filepath.Join(root, "mounted")
	require.NoError(t, os.Mkdir(hostDir, 0o700))
	require.NoError(t, os.Mkdir(mountedDir, 0o700))
	return hostDir, mountedDir
}

func bindAtomicMount(t *testing.T, source, target string, readOnly bool) {
	t.Helper()
	require.NoError(t, unix.Mount(source, target, "", unix.MS_BIND, ""), "enabled mount test requires a working bind mount")
	t.Cleanup(func() {
		require.NoError(t, unix.Unmount(target, 0), "unmount test fixture")
	})
	if readOnly {
		require.NoError(t, unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""))
		var stat unix.Statfs_t
		require.NoError(t, unix.Statfs(target, &stat))
		require.NotZero(t, stat.Flags&unix.ST_RDONLY, "fixture must actually be a read-only mount")
	}
}

func writeAtomicMountOriginal(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, data, 0o640))
	// Non-root ownership catches a replacement that accidentally inherits the
	// privileged writer's credentials instead of preserving the original owner.
	require.NoError(t, os.Chown(path, 12345, 23456))
	require.NoError(t, os.Chmod(path, 0o640))
}

func atomicMountCandidate(t *testing.T) (*NetworkPolicy, []byte) {
	t.Helper()
	p, err := ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"new.example.com"}]}`)
	require.NoError(t, err)
	data, err := json.MarshalIndent(p, "", "  ")
	require.NoError(t, err)
	return p, append(data, '\n')
}

func assertAtomicMountRejected(t *testing.T, path string) {
	t.Helper()
	store, err := NewAtomicPolicyFile(path)
	if err != nil {
		return // Rejecting the unsupported target at construction is also safe.
	}
	require.NotNil(t, store)
	candidate, _ := atomicMountCandidate(t)
	state, err := store.Save(candidate)
	require.Error(t, err, "unsupported mount or symlink must be rejected")
	require.Equal(t, FileUnchanged, state, "rejection must establish that the live file was not mutated")
}

type atomicMountFile struct {
	bytes []byte
	mode  os.FileMode
	uid   uint32
	gid   uint32
	dev   uint64
	inode uint64
}

func readAtomicMountFile(t *testing.T, path string) atomicMountFile {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return atomicMountFile{bytes: data, mode: info.Mode(), uid: stat.Uid, gid: stat.Gid, dev: stat.Dev, inode: stat.Ino}
}

func assertAtomicMountMetadata(t *testing.T, expected, actual atomicMountFile) {
	t.Helper()
	require.Equal(t, expected.mode, actual.mode, "preserve file mode")
	require.Equal(t, expected.uid, actual.uid, "preserve file owner")
	require.Equal(t, expected.gid, actual.gid, "preserve file group")
}

func assertAtomicMountEntries(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		actual = append(actual, entry.Name())
	}
	require.ElementsMatch(t, names, actual, "no policy staging files should remain")
}
