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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

var errAtomicInjected = errors.New("injected I/O failure")

type faultyAtomicTemp struct {
	atomicTempFile
	phase string
	calls *[]string
}

func (f *faultyAtomicTemp) Write(data []byte) (int, error) {
	*f.calls = append(*f.calls, "write")
	if f.phase == "write" || f.phase == "short-write" {
		n, err := f.atomicTempFile.Write(data[:len(data)/2])
		if err != nil {
			return n, err
		}
		if f.phase == "short-write" {
			return n, nil
		}
		return n, errAtomicInjected
	}
	return f.atomicTempFile.Write(data)
}
func (f *faultyAtomicTemp) Chown(uid, gid int) error {
	*f.calls = append(*f.calls, "ownership")
	if f.phase == "ownership" {
		return errAtomicInjected
	}
	return f.atomicTempFile.Chown(uid, gid)
}
func (f *faultyAtomicTemp) Chmod(mode fs.FileMode) error {
	*f.calls = append(*f.calls, "mode")
	if f.phase == "mode" {
		return errAtomicInjected
	}
	return f.atomicTempFile.Chmod(mode)
}
func (f *faultyAtomicTemp) Sync() error {
	*f.calls = append(*f.calls, "file-sync")
	if f.phase == "file-sync" {
		return errAtomicInjected
	}
	return f.atomicTempFile.Sync()
}
func (f *faultyAtomicTemp) Close() error {
	*f.calls = append(*f.calls, "file-close")
	err := f.atomicTempFile.Close()
	if f.phase == "file-close" {
		return errors.Join(err, errAtomicInjected)
	}
	return err
}

func injectAtomicPhase(s *AtomicPolicyFile, phase string, calls *[]string) {
	real := s.ops
	s.ops.openDir = func(path string) (*os.File, error) {
		*calls = append(*calls, "validate-directory")
		if phase == "validate-directory" {
			return nil, errAtomicInjected
		}
		return real.openDir(path)
	}
	s.ops.snapshot = func(dir *os.File, name, path string) (*PolicyFileSnapshot, error) {
		*calls = append(*calls, "snapshot")
		if phase == "snapshot" {
			return nil, errAtomicInjected
		}
		return real.snapshot(dir, name, path)
	}
	s.ops.create = func(dir *os.File) (atomicTempFile, error) {
		*calls = append(*calls, "create")
		if phase == "create" {
			return nil, errAtomicInjected
		}
		f, err := real.create(dir)
		if err != nil {
			return nil, err
		}
		return &faultyAtomicTemp{atomicTempFile: f, phase: phase, calls: calls}, nil
	}
	s.ops.rename = func(dir *os.File, old, new string) error {
		*calls = append(*calls, "rename")
		if phase == "rename-before" {
			return errAtomicInjected
		}
		err := real.rename(dir, old, new)
		if phase == "rename-after" {
			return errors.Join(err, errAtomicInjected)
		}
		return err
	}
	s.ops.syncDir = func(dir *os.File) error {
		*calls = append(*calls, "directory-sync")
		if phase == "directory-sync" {
			return errAtomicInjected
		}
		return real.syncDir(dir)
	}
	s.ops.openFinal = func(dir *os.File, name string) (*os.File, error) {
		*calls = append(*calls, "final-open")
		if phase == "final-open" {
			return nil, errAtomicInjected
		}
		return real.openFinal(dir, name)
	}
	s.ops.security = func(fd int) (policyFileSecurity, error) {
		*calls = append(*calls, "final-security")
		if phase == "final-security" {
			return policyFileSecurity{}, errAtomicInjected
		}
		return real.security(fd)
	}
	s.ops.syncFinal = func(f *os.File) error {
		*calls = append(*calls, "final-sync")
		if phase == "final-sync" {
			return errAtomicInjected
		}
		return real.syncFinal(f)
	}
	s.ops.closeFinal = func(f *os.File) error {
		*calls = append(*calls, "final-close")
		err := real.closeFinal(f)
		if phase == "final-close" {
			return errors.Join(err, errAtomicInjected)
		}
		return err
	}
	s.ops.closeDir = func(dir *os.File) error {
		*calls = append(*calls, "directory-close")
		err := real.closeDir(dir)
		if phase == "directory-close" {
			return errors.Join(err, errAtomicInjected)
		}
		return err
	}
}

func TestAtomicPolicyFileMutationFaults(t *testing.T) {
	phases := []string{"validate-directory", "snapshot", "create", "write", "short-write", "ownership", "mode", "file-sync", "file-close", "final-open", "final-security", "final-sync", "final-close", "rename-before", "rename-after", "directory-sync", "directory-close"}
	for _, operation := range []string{"save", "restore"} {
		for _, exists := range []bool{false, true} {
			if operation == "restore" && !exists {
				continue
			} // unlink protocol is covered separately
			for _, phase := range phases {
				if phase == "ownership" && !exists {
					continue
				}
				t.Run(operation+"/"+map[bool]string{true: "existing", false: "absent"}[exists]+"/"+phase, func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "policy.json")
					old := []byte("  exact original bytes\x00\n")
					if exists {
						require.NoError(t, os.WriteFile(path, old, 0o640))
					}
					s, err := NewAtomicPolicyFile(path)
					require.NoError(t, err)
					snapshot, err := s.Snapshot()
					require.NoError(t, err)
					before, desired := old, atomicJSON(t, DefaultDenyPolicy())
					if !exists {
						before = nil
					}
					if operation == "restore" {
						state, err := s.Save(DefaultDenyPolicy())
						require.NoError(t, err)
						require.Equal(t, FileCommitted, state)
						before, desired = desired, old
					}
					var calls []string
					injectAtomicPhase(s, phase, &calls)
					var state FileMutationState
					if operation == "save" {
						state, err = s.Save(DefaultDenyPolicy())
					} else {
						state, err = s.Restore(snapshot)
					}
					require.Error(t, err)
					if phase == "short-write" {
						require.ErrorIs(t, err, io.ErrShortWrite)
					} else {
						require.ErrorIs(t, err, errAtomicInjected)
					}
					var phaseErr *FileMutationError
					require.ErrorAs(t, err, &phaseErr)
					require.NotContains(t, err.Error(), string(old))
					unknown := strings.HasPrefix(phase, "rename-") || strings.HasPrefix(phase, "directory-")
					if unknown {
						require.Equal(t, FileUnknown, state)
					} else {
						require.Equal(t, FileUnchanged, state)
						require.NotContains(t, calls, "rename")
					}
					published := phase == "rename-after" || phase == "directory-sync" || phase == "directory-close"
					want := before
					if published {
						want = desired
					}
					assertAtomicFileImage(t, path, want, exists || published, map[bool]fs.FileMode{true: 0o640, false: 0o600}[exists])
					assertNoAtomicTemps(t, dir)
				})
			}
		}
	}
}

func TestAtomicPolicyFileRoundTripExactAndMetadata(t *testing.T) {
	for _, mode := range []fs.FileMode{0o600, 0o640, 0o444, 0o750 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "policy.json")
			old := []byte("\n non-JSON snapshot \x00 \n")
			require.NoError(t, os.WriteFile(path, old, 0o600))
			require.NoError(t, os.Chmod(path, mode))
			original, err := os.Open(path)
			require.NoError(t, err)
			defer original.Close()
			s, err := NewAtomicPolicyFile(path)
			require.NoError(t, err)
			snapshot, err := s.Snapshot()
			require.NoError(t, err)
			var calls []string
			injectAtomicPhase(s, "", &calls)
			state, err := s.Save(nil)
			require.NoError(t, err)
			require.Equal(t, FileCommitted, state)
			require.Equal(t, []string{"validate-directory", "snapshot", "create", "write", "ownership", "mode", "file-sync", "file-close", "final-open", "final-security", "final-sync", "final-close", "rename", "directory-sync", "directory-close"}, calls)
			assertAtomicFileImage(t, path, atomicJSON(t, DefaultDenyPolicy()), true, mode)
			oldStill, err := io.ReadAll(original)
			require.NoError(t, err)
			require.Equal(t, old, oldStill, "open original inode must never be truncated")
			info, err := original.Stat()
			require.NoError(t, err)
			require.Equal(t, mode, info.Mode())
			state, err = s.Restore(snapshot)
			require.NoError(t, err)
			require.Equal(t, FileCommitted, state)
			assertAtomicFileImage(t, path, old, true, mode)
			assertNoAtomicTemps(t, dir)
		})
	}
}

func TestAtomicPolicyFileRestoreAbsent(t *testing.T) {
	for _, phase := range []string{"", "unlink-before", "unlink-after", "directory-sync", "directory-close"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.json")
			s, err := NewAtomicPolicyFile(path)
			require.NoError(t, err)
			snapshot, err := s.Snapshot()
			require.NoError(t, err)
			state, err := s.Save(nil)
			require.NoError(t, err)
			require.Equal(t, FileCommitted, state)
			assertAtomicFileImage(t, path, atomicJSON(t, DefaultDenyPolicy()), true, 0o600)
			var calls []string
			injectAtomicPhase(s, phase, &calls)
			remove := s.ops.remove
			s.ops.remove = func(dir *os.File, name string) error {
				if phase == "unlink-before" {
					return errAtomicInjected
				}
				err := remove(dir, name)
				if phase == "unlink-after" {
					return errors.Join(err, errAtomicInjected)
				}
				return err
			}
			state, err = s.Restore(snapshot)
			if phase == "" {
				require.NoError(t, err)
				require.Equal(t, FileCommitted, state)
			} else {
				require.ErrorIs(t, err, errAtomicInjected)
				require.Equal(t, FileUnknown, state)
			}
			if phase == "unlink-before" {
				assertAtomicFileImage(t, path, atomicJSON(t, DefaultDenyPolicy()), true, 0o600)
			} else {
				assertAtomicFileImage(t, path, nil, false, 0)
			}
			assertNoAtomicTemps(t, filepath.Dir(path))
		})
	}
}

func TestAtomicPolicyFileRestoreAlreadyAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)
	snapshot, err := s.Snapshot()
	require.NoError(t, err)
	s.ops.remove = func(*os.File, string) error { t.Fatal("absent path must not be unlinked"); return nil }
	var calls []string
	injectAtomicPhase(s, "directory-sync", &calls)
	state, err := s.Restore(snapshot)
	require.ErrorIs(t, err, errAtomicInjected)
	require.Equal(t, FileUnchanged, state)
	require.Contains(t, calls, "directory-sync")
}

func TestAtomicPolicyFileCleanupFailureIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	old := []byte("original")
	require.NoError(t, os.WriteFile(path, old, 0o600))
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)
	var calls []string
	injectAtomicPhase(s, "write", &calls)
	s.ops.remove = func(*os.File, string) error { return errors.New("cleanup failed") }
	state, err := s.Save(nil)
	require.ErrorIs(t, err, errAtomicInjected)
	require.ErrorContains(t, err, "temporary-cleanup")
	require.Equal(t, FileUnchanged, state)
	assertAtomicFileImage(t, path, old, true, 0o600)
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 2, "failed cleanup must be visible, never silently ignored")
}

func TestAtomicPolicyFileRejectsUnsupportedPaths(t *testing.T) {
	for _, layout := range []string{"file-symlink", "ancestor-symlink", "directory", "fifo", "hardlink", "parent-group-writable", "parent-world-writable", "parent-readonly", "untrusted-ancestor", "missing-parent", "traversal", "empty"} {
		t.Run(layout, func(t *testing.T) {
			base := t.TempDir()
			parent := filepath.Join(base, "owned")
			require.NoError(t, os.Mkdir(parent, 0o700))
			path := filepath.Join(parent, "policy.json")
			old := []byte("original bytes")
			target := filepath.Join(base, "target.json")
			require.NoError(t, os.WriteFile(target, old, 0o640))
			switch layout {
			case "file-symlink":
				require.NoError(t, os.Symlink(target, path))
			case "ancestor-symlink":
				link := filepath.Join(base, "link")
				require.NoError(t, os.Symlink(parent, link))
				path = filepath.Join(link, "policy.json")
			case "directory":
				require.NoError(t, os.Mkdir(path, 0o700))
			case "fifo":
				require.NoError(t, unix.Mkfifo(path, 0o600))
			case "hardlink":
				require.NoError(t, os.Link(target, path))
			case "parent-group-writable":
				require.NoError(t, os.Chmod(parent, 0o770))
			case "parent-world-writable":
				require.NoError(t, os.Chmod(parent, 0o707))
			case "parent-readonly":
				require.NoError(t, os.Chmod(parent, 0o500))
				defer os.Chmod(parent, 0o700)
			case "untrusted-ancestor":
				require.NoError(t, os.Chmod(base, 0o777))
				defer os.Chmod(base, 0o700)
			case "missing-parent":
				path = filepath.Join(base, "missing", "policy.json")
			case "traversal":
				path = parent + "/../target.json"
			case "empty":
				path = " "
			}
			s, err := NewAtomicPolicyFile(path)
			require.Error(t, err)
			require.Nil(t, s)
			assertAtomicFileImage(t, target, old, true, 0o640)
		})
	}
}

func TestAtomicPolicyFileRevalidatesBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)
	snapshot, err := s.Snapshot()
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "target.json")
	require.NoError(t, os.WriteFile(target, []byte("do not touch"), 0o640))
	require.NoError(t, os.Symlink(target, path))
	for _, operation := range []func() (FileMutationState, error){func() (FileMutationState, error) { return s.Save(nil) }, func() (FileMutationState, error) { return s.Restore(snapshot) }} {
		state, err := operation()
		require.Error(t, err)
		require.Equal(t, FileUnchanged, state)
		assertAtomicFileImage(t, target, []byte("do not touch"), true, 0o640)
	}
}

func TestAtomicPolicyFileSnapshotAndWrongSnapshotFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)
	other, err := NewAtomicPolicyFile(filepath.Join(t.TempDir(), "other.json"))
	require.NoError(t, err)
	wrong, err := other.Snapshot()
	require.NoError(t, err)
	for _, snapshot := range []*PolicyFileSnapshot{nil, wrong} {
		state, err := s.Restore(snapshot)
		require.ErrorContains(t, err, "validate-snapshot")
		require.Equal(t, FileUnchanged, state)
	}
	var calls []string
	injectAtomicPhase(s, "directory-close", &calls)
	snapshot, err := s.Snapshot()
	require.Nil(t, snapshot)
	require.ErrorIs(t, err, errAtomicInjected)
	assertAtomicFileImage(t, path, nil, false, 0)
}

func atomicJSON(t *testing.T, p *NetworkPolicy) []byte {
	t.Helper()
	data, err := json.MarshalIndent(p, "", "  ")
	require.NoError(t, err)
	return append(data, '\n')
}

func assertAtomicFileImage(t *testing.T, path string, want []byte, exists bool, mode fs.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if !exists {
		require.ErrorIs(t, err, fs.ErrNotExist)
		return
	}
	require.NoError(t, err)
	require.True(t, bytes.Equal(want, data), "unexpected authoritative file bytes: got %q, want %q", data, want)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode, info.Mode())
	var st unix.Stat_t
	require.NoError(t, unix.Stat(path, &st))
	require.Equal(t, uint32(os.Geteuid()), st.Uid)
	require.Equal(t, uint32(os.Getegid()), st.Gid)
}

func assertNoAtomicTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), ".policy-"), "temporary file leaked: %s", entry.Name())
	}
}

func TestAtomicPolicyFileRejectsExtendedAttributes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	original := []byte("labeled policy")
	require.NoError(t, os.WriteFile(path, original, 0o640))
	err := unix.Setxattr(path, "user.atomic-policy-test", []byte("preserve me"), 0)
	if errors.Is(err, unix.ENOTSUP) {
		t.Skip("filesystem does not support xattrs")
	}
	require.NoError(t, err)
	s, err := NewAtomicPolicyFile(path)
	require.Nil(t, s)
	require.ErrorContains(t, err, "extended attributes")
	assertAtomicFileImage(t, path, original, true, 0o640)
	buf := make([]byte, 64)
	n, err := unix.Getxattr(path, "user.atomic-policy-test", buf)
	require.NoError(t, err)
	require.Equal(t, "preserve me", string(buf[:n]))
}

func TestAtomicPolicyFileRejectsDirectoryACLs(t *testing.T) {
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "policy.json")
			// Linux POSIX ACL xattr version 2, owner=rwx, named user=r-x,
			// group=r-x, mask=r-x, other=r-x. The mode is 0755, so checking
			// only group/world-writable bits cannot detect this ACL.
			data := []byte{2, 0, 0, 0}
			for _, entry := range [][3]uint32{{1, 7, 0xffffffff}, {2, 5, uint32(os.Geteuid())}, {4, 5, 0xffffffff}, {16, 5, 0xffffffff}, {32, 5, 0xffffffff}} {
				data = append(data, byte(entry[0]), byte(entry[0]>>8), byte(entry[1]), byte(entry[1]>>8), byte(entry[2]), byte(entry[2]>>8), byte(entry[2]>>16), byte(entry[2]>>24))
			}
			err := unix.Setxattr(dir, name, data, 0)
			if errors.Is(err, unix.ENOTSUP) {
				t.Skip("filesystem does not support ACLs")
			}
			require.NoError(t, err)
			defer unix.Removexattr(dir, name)
			s, err := NewAtomicPolicyFile(path)
			require.Nil(t, s)
			require.ErrorContains(t, err, "ACL")
			assertAtomicFileImage(t, path, nil, false, 0)
		})
	}
}

func TestAtomicPolicyMountIdentityParsing(t *testing.T) {
	id, err := parsePolicyMountID([]byte("pos:\t0\nflags:\t0100000\nmnt_id:\t12345\nino:\t99\n"))
	require.NoError(t, err)
	require.Equal(t, uint64(12345), id)
	for _, raw := range []string{"", "ino:\t12345\n", "mnt_id:\n", "mnt_id:\t0\n", "mnt_id:\t-1\n", "mnt_id:\tbad\n", "mnt_id:\t1 2\n", "mnt_id:\t1\nmnt_id:\t1\n", "mnt_id:\t18446744073709551616\n"} {
		t.Run(raw, func(t *testing.T) {
			_, err := parsePolicyMountID([]byte(raw))
			require.Error(t, err)
		})
	}
	f, err := os.Open(t.TempDir())
	require.NoError(t, err)
	defer f.Close()
	data, err := os.ReadFile("/proc/self/fdinfo/" + fmt.Sprint(f.Fd()))
	require.NoError(t, err)
	fromProc, err := parsePolicyMountID(data)
	require.NoError(t, err)
	fromFD, err := policyMountID(int(f.Fd()))
	require.NoError(t, err)
	require.Equal(t, fromProc, fromFD)
}

func TestAtomicPolicyFileRestoresSnapshotModeNotCurrentMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	original := []byte("original")
	require.NoError(t, os.WriteFile(path, original, 0o640))
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)
	snapshot, err := s.Snapshot()
	require.NoError(t, err)
	state, err := s.Save(nil)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	require.NoError(t, os.Chmod(path, 0o600))
	state, err = s.Restore(snapshot)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state)
	assertAtomicFileImage(t, path, original, true, 0o640)
}

func TestAtomicPolicyFilePreRenameAndCloseFailureRemainUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	original := []byte("original")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	s, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)
	var calls []string
	injectAtomicPhase(s, "write", &calls)
	s.ops.closeDir = func(dir *os.File) error { return errors.Join(dir.Close(), errors.New("directory close failed")) }
	state, err := s.Save(nil)
	require.ErrorIs(t, err, errAtomicInjected)
	require.ErrorContains(t, err, "directory-close")
	require.Equal(t, FileUnchanged, state)
	assertAtomicFileImage(t, path, original, true, 0o600)
	assertNoAtomicTemps(t, filepath.Dir(path))
}

func TestAtomicPolicyFileDirectorySyncPreflight(t *testing.T) {
	for _, syncErr := range []error{unix.EINVAL, unix.ENOTSUP} {
		for _, exists := range []bool{false, true} {
			t.Run(syncErr.Error()+"/"+map[bool]string{false: "absent", true: "existing"}[exists], func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "policy.json")
				original := []byte("exact original\x00\n")
				if exists {
					require.NoError(t, os.WriteFile(path, original, 0o640))
				}
				ops := defaultAtomicFileOps()
				syncs, closes := 0, 0
				ops.syncDir = func(*os.File) error { syncs++; return syncErr }
				closeDir := ops.closeDir
				ops.closeDir = func(f *os.File) error { closes++; return closeDir(f) }
				ops.create = func(*os.File) (atomicTempFile, error) { t.Fatal("preflight must not create a file"); return nil, nil }
				ops.rename = func(*os.File, string, string) error { t.Fatal("preflight must not rename"); return nil }
				store, err := newAtomicPolicyFile(path, ops)
				require.Nil(t, store)
				require.ErrorIs(t, err, syncErr)
				require.ErrorContains(t, err, "directory-sync-preflight")
				require.Equal(t, 1, syncs)
				require.Equal(t, 1, closes)
				assertAtomicFileImage(t, path, original, exists, 0o640)
				assertNoAtomicTemps(t, dir)

				// An owner revalidating an already constructed store must also
				// discover a lost capability before it starts external effects.
				store, err = NewAtomicPolicyFile(path)
				require.NoError(t, err)
				store.ops = ops
				snapshot, err := store.Snapshot()
				require.Nil(t, snapshot)
				require.ErrorIs(t, err, syncErr)
				require.Equal(t, 2, syncs)
				require.Equal(t, 2, closes)
				assertAtomicFileImage(t, path, original, exists, 0o640)
				assertNoAtomicTemps(t, dir)
			})
		}
	}
}

// These fixtures emulate xattr syscall results, not SELinux enforcement or
// IMA/EVM generation. Native policy-specific validation requires a labeled host.
func inspectFakePolicySecurity(attrs map[string][]byte) (policyFileSecurity, error) {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var names []byte
	for _, k := range keys {
		names = append(names, []byte(k)...)
		names = append(names, 0)
	}
	copyValue := func(buf, value []byte) (int, error) {
		if buf == nil {
			return len(value), nil
		}
		if len(buf) < len(value) {
			return 0, unix.ERANGE
		}
		return copy(buf, value), nil
	}
	return inspectPolicyFileSecurity(func(buf []byte) (int, error) { return copyValue(buf, names) }, func(name string, buf []byte) (int, error) {
		value, ok := attrs[name]
		if !ok {
			return 0, unix.ENODATA
		}
		return copyValue(buf, value)
	})
}

func TestAtomicPolicyManagedSecurityClassification(t *testing.T) {
	ima := append([]byte{0x04, 0x04}, bytes.Repeat([]byte{0x5a}, 32)...)
	evm := append([]byte{0x02}, bytes.Repeat([]byte{0x6b}, 20)...)
	for _, tc := range []struct {
		name    string
		attrs   map[string][]byte
		allowed bool
	}{
		{"empty", nil, true},
		{"selinux", map[string][]byte{"security.selinux": []byte("system_u:object_r:policy_t:s0\x00")}, true},
		{"ima-digest", map[string][]byte{"security.ima": append([]byte{0x01}, bytes.Repeat([]byte{1}, 20)...)}, true},
		{"ima-digest-ng", map[string][]byte{"security.ima": ima}, true},
		{"combined", map[string][]byte{"security.selinux": []byte("label\x00"), "security.ima": ima, "security.evm": evm}, true},
		{"user", map[string][]byte{"user.note": []byte("reject")}, false},
		{"acl", map[string][]byte{"system.posix_acl_access": []byte("reject")}, false},
		{"default-acl", map[string][]byte{"system.posix_acl_default": []byte("reject")}, false},
		{"capability", map[string][]byte{"security.capability": []byte("reject")}, false},
		{"unknown-security", map[string][]byte{"security.unknown": []byte("reject")}, false},
		{"empty-selinux", map[string][]byte{"security.selinux": nil}, false},
		{"ima-signature", map[string][]byte{"security.ima": []byte{0x03, 1, 2}}, false},
		{"ima-verity-signature", map[string][]byte{"security.ima": []byte{0x06, 1, 2}}, false},
		{"ima-unknown", map[string][]byte{"security.ima": []byte{0xff, 1, 2}}, false},
		{"ima-empty", map[string][]byte{"security.ima": nil}, false},
		{"ima-short-ng", map[string][]byte{"security.ima": []byte{0x04, 1}}, false},
		{"evm-signature", map[string][]byte{"security.evm": []byte{0x03, 1, 2}}, false},
		{"evm-portable-signature", map[string][]byte{"security.evm": []byte{0x05, 1, 2}}, false},
		{"evm-short-hmac", map[string][]byte{"security.evm": []byte{0x02, 1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata, err := inspectFakePolicySecurity(tc.attrs)
			if !tc.allowed {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, string(tc.attrs["security.selinux"]), metadata.selinux)
			_, ima := tc.attrs["security.ima"]
			_, evm := tc.attrs["security.evm"]
			require.Equal(t, ima, metadata.ima)
			require.Equal(t, evm, metadata.evm)
		})
	}
}

func TestAtomicPolicyManagedSecurityRoundTrips(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[exists], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.json")
			original := []byte("exact original\x00\n")
			if exists {
				require.NoError(t, os.WriteFile(path, original, 0640))
			}
			const label = "system_u:object_r:policy_t:s0\x00"
			writerChecks, finalChecks := 0, 0
			reader := func(fd int) (policyFileSecurity, error) {
				name, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
				if err != nil {
					return policyFileSecurity{}, err
				}
				flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
				if err != nil {
					return policyFileSecurity{}, err
				}
				if strings.HasPrefix(filepath.Base(name), ".policy-") {
					if flags&unix.O_ACCMODE == unix.O_WRONLY {
						writerChecks++
						return policyFileSecurity{selinux: label}, nil
					}
					finalChecks++
					require.Equal(t, unix.O_RDONLY, flags&unix.O_ACCMODE)
				}
				// Simulate kernel-generated digests becoming available on last
				// writer close. No original signature/digest payload is stored.
				return policyFileSecurity{selinux: label, ima: true, evm: true}, nil
			}
			store, err := newAtomicPolicyFile(path, atomicFileOpsWithSecurity(reader))
			require.NoError(t, err)
			snapshot, err := store.Snapshot()
			require.NoError(t, err)
			for i := 0; i < 2; i++ {
				state, err := store.Save(DefaultDenyPolicy())
				require.NoError(t, err)
				require.Equal(t, FileCommitted, state)
			}
			state, err := store.Restore(snapshot)
			require.NoError(t, err)
			require.Equal(t, FileCommitted, state)
			state, err = store.Restore(snapshot)
			require.NoError(t, err)
			require.Equal(t, FileCommitted, state)
			if exists {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, original, data)
			} else {
				_, err := os.Stat(path)
				require.ErrorIs(t, err, fs.ErrNotExist)
			}
			state, err = store.Save(DefaultDenyPolicy())
			require.NoError(t, err)
			require.Equal(t, FileCommitted, state)
			again, err := store.Snapshot()
			require.NoError(t, err)
			require.True(t, again.exists)
			require.Equal(t, policyFileSecurity{selinux: label, ima: true, evm: true}, again.security)
			want := 3
			if exists {
				want = 5
			}
			require.Equal(t, want, writerChecks)
			require.Equal(t, want, finalChecks)
			assertNoAtomicTemps(t, filepath.Dir(path))
		})
	}
}

func TestAtomicPolicyManagedSecurityMismatchUnchanged(t *testing.T) {
	for _, kind := range []string{"different-selinux", "lost-selinux", "gained-selinux", "missing-ima", "missing-evm", "late-signature", "late-user-attribute"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "policy.json")
			original := []byte("original")
			require.NoError(t, os.WriteFile(path, original, 0640))
			before, err := os.Stat(path)
			require.NoError(t, err)
			old := policyFileSecurity{selinux: "target-label", ima: true, evm: true}
			if kind == "gained-selinux" {
				old.selinux = ""
			}
			reader := func(fd int) (policyFileSecurity, error) {
				name, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
				if err != nil {
					return policyFileSecurity{}, err
				}
				flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
				if err != nil {
					return policyFileSecurity{}, err
				}
				if !strings.HasPrefix(filepath.Base(name), ".policy-") || flags&unix.O_ACCMODE == unix.O_WRONLY {
					return old, nil
				}
				next := old
				switch kind {
				case "different-selinux":
					next.selinux = "temp-name-label"
				case "lost-selinux":
					next.selinux = ""
				case "gained-selinux":
					next.selinux = "new-label"
				case "missing-ima":
					next.ima = false
				case "missing-evm":
					next.evm = false
				case "late-signature":
					return inspectFakePolicySecurity(map[string][]byte{"security.ima": {0x03, 1, 2}})
				case "late-user-attribute":
					return inspectFakePolicySecurity(map[string][]byte{"user.late": []byte("reject")})
				}
				return next, nil
			}
			store, err := newAtomicPolicyFile(path, atomicFileOpsWithSecurity(reader))
			require.NoError(t, err)
			snapshot, err := store.Snapshot()
			require.NoError(t, err)
			for _, restore := range []bool{false, true} {
				var state FileMutationState
				if restore {
					state, err = store.Restore(snapshot)
				} else {
					state, err = store.Save(DefaultDenyPolicy())
				}
				require.Error(t, err)
				require.Equal(t, FileUnchanged, state)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, original, data)
				after, err := os.Stat(path)
				require.NoError(t, err)
				require.True(t, os.SameFile(before, after))
				assertNoAtomicTemps(t, filepath.Dir(path))
			}
		})
	}
}

func TestAtomicPolicyFinalReopenIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0600))
	store, err := NewAtomicPolicyFile(path)
	require.NoError(t, err)
	realOpen := store.ops.openFinal
	store.ops.openFinal = func(dir *os.File, name string) (*os.File, error) {
		// A name now referring to a foreign inode must never be published.
		return realOpen(dir, "policy.json")
	}
	state, err := store.Save(DefaultDenyPolicy())
	require.ErrorContains(t, err, "final-identity")
	require.Equal(t, FileUnchanged, state)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "original", string(data))
	assertNoAtomicTemps(t, filepath.Dir(path))
}

func TestAtomicPolicyRestoreUsesSnapshotSELinuxLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	require.NoError(t, os.WriteFile(path, []byte("snapshot bytes"), 0o600))
	currentLabel := "original-label"
	reader := func(fd int) (policyFileSecurity, error) {
		name, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
		if err != nil {
			return policyFileSecurity{}, err
		}
		if strings.HasPrefix(filepath.Base(name), ".policy-") {
			return policyFileSecurity{selinux: "original-label"}, nil
		}
		return policyFileSecurity{selinux: currentLabel}, nil
	}
	store, err := newAtomicPolicyFile(path, atomicFileOpsWithSecurity(reader))
	require.NoError(t, err)
	snapshot, err := store.Snapshot()
	require.NoError(t, err)
	currentLabel = "current-label"
	state, err := store.Restore(snapshot)
	require.NoError(t, err)
	require.Equal(t, FileCommitted, state, "restore compares with the requested snapshot, not the current file")
}

func TestAtomicPolicyXattrInspectionErrors(t *testing.T) {
	for _, cause := range []error{unix.EACCES, unix.EIO} {
		_, err := inspectPolicyFileSecurity(func([]byte) (int, error) { return 0, cause }, nil)
		require.ErrorIs(t, err, cause)
	}
	security, err := inspectPolicyFileSecurity(func([]byte) (int, error) { return 0, unix.ENOTSUP }, nil)
	require.NoError(t, err)
	require.Equal(t, policyFileSecurity{}, security)
	for _, names := range []string{"missing-nul", "\x00", "security.ima\x00security.ima\x00"} {
		_, err := inspectPolicyFileSecurity(func(buf []byte) (int, error) {
			if buf == nil {
				return len(names), nil
			}
			return copy(buf, names), nil
		}, func(string, []byte) (int, error) { return 2, nil })
		require.Error(t, err)
	}
	_, err = inspectPolicyFileSecurity(func(buf []byte) (int, error) {
		name := "security.selinux\x00"
		if buf == nil {
			return len(name), nil
		}
		return copy(buf, name), nil
	}, func(string, []byte) (int, error) { return 0, unix.ENODATA })
	require.ErrorIs(t, err, unix.ENODATA, "disappearing attributes must not become success")
	calls := 0
	value, err := readPolicyXattrBytes(func(buf []byte) (int, error) {
		calls++
		if buf == nil {
			return 3, nil
		}
		if calls == 2 {
			return 0, unix.ERANGE
		}
		return copy(buf, "new"), nil
	})
	require.NoError(t, err)
	require.Equal(t, "new", string(value))
	require.Equal(t, 4, calls)
	calls = 0
	_, err = readPolicyXattrBytes(func(buf []byte) (int, error) {
		calls++
		if buf == nil {
			return 3, nil
		}
		return 0, unix.ERANGE
	})
	require.ErrorContains(t, err, "changed during inspection")
	require.Equal(t, 6, calls)
	_, err = readPolicyXattrBytes(func([]byte) (int, error) { return 65537, nil })
	require.Error(t, err)
}
