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

//go:build linux

package runtime

import (
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFilesystemCredentialUnknownUserDoesNotInheritGroups(t *testing.T) {
	const uid = uint32(4294967294)
	if _, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil {
		t.Skip("test UID belongs to a real account")
	}
	cred, home, err := filesystemCredential(uid, 41000)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, uid, cred.Uid)
	require.Equal(t, uint32(41000), cred.Gid)
	require.Empty(t, cred.Groups)
	require.Equal(t, "/", home)
}

func TestFilesystemCredentialResolvesAccountGroups(t *testing.T) {
	uid := uint32(os.Getuid())
	account, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	require.NoError(t, err)
	groups, err := account.GroupIds()
	require.NoError(t, err)
	expected := make([]uint32, 0, len(groups))
	for _, group := range groups {
		id, err := strconv.ParseUint(group, 10, 32)
		require.NoError(t, err)
		expected = append(expected, uint32(id))
	}
	// A different explicit primary GID forces a non-nil credential while
	// supplementary groups must still come from the selected account.
	cred, home, err := filesystemCredential(uid, 4294967294)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, uint32(4294967294), cred.Gid)
	require.ElementsMatch(t, expected, cred.Groups)
	require.Equal(t, account.HomeDir, home)
}

func stubFilesystemLookups(
	t *testing.T,
	lookupUser func(string) (*user.User, error),
	lookupGroups func(*user.User) ([]string, error),
) {
	t.Helper()
	prevUser, prevGroups := lookupFilesystemUser, lookupFilesystemGroups
	t.Cleanup(func() {
		lookupFilesystemUser, lookupFilesystemGroups = prevUser, prevGroups
	})
	lookupFilesystemUser = lookupUser
	lookupFilesystemGroups = lookupGroups
}

func TestFilesystemCredentialMissingPasswdFileActsAsUnknownUser(t *testing.T) {
	stubFilesystemLookups(t,
		func(string) (*user.User, error) {
			return nil, &fs.PathError{Op: "open", Path: "/etc/passwd", Err: fs.ErrNotExist}
		},
		func(*user.User) ([]string, error) {
			t.Fatal("groups must not be looked up without an account")
			return nil, nil
		},
	)
	cred, home, err := filesystemCredential(41500, 41501)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, uint32(41500), cred.Uid)
	require.Equal(t, uint32(41501), cred.Gid)
	require.Empty(t, cred.Groups)
	require.Equal(t, "/", home)
}

func TestFilesystemCredentialMissingGroupFileUsesNoSupplementaryGroups(t *testing.T) {
	stubFilesystemLookups(t,
		func(uid string) (*user.User, error) {
			return &user.User{Uid: uid, Gid: "41500", Username: "agent", HomeDir: "/home/agent"}, nil
		},
		func(*user.User) ([]string, error) {
			return nil, &fs.PathError{Op: "open", Path: "/etc/group", Err: fs.ErrNotExist}
		},
	)
	cred, home, err := filesystemCredential(41500, 41501)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, uint32(41500), cred.Uid)
	require.Equal(t, uint32(41501), cred.Gid)
	require.Empty(t, cred.Groups)
	require.Equal(t, "/home/agent", home)
}

func TestFilesystemCredentialRejectsUnreadableAccountFiles(t *testing.T) {
	permission := &fs.PathError{Op: "open", Path: "/etc/passwd", Err: fs.ErrPermission}
	stubFilesystemLookups(t,
		func(string) (*user.User, error) { return nil, permission },
		func(*user.User) ([]string, error) { return nil, nil },
	)
	_, _, err := filesystemCredential(41500, 41501)
	require.ErrorIs(t, err, fs.ErrPermission)

	stubFilesystemLookups(t,
		func(uid string) (*user.User, error) {
			return &user.User{Uid: uid, Gid: "41500", HomeDir: "/home/agent"}, nil
		},
		func(*user.User) ([]string, error) { return nil, permission },
	)
	_, _, err = filesystemCredential(41500, 41501)
	require.ErrorIs(t, err, fs.ErrPermission)
}

func TestFilesystemCredentialRejectsReservedIDs(t *testing.T) {
	for _, pair := range [][2]uint32{{^uint32(0), 0}, {0, ^uint32(0)}} {
		_, _, err := filesystemCredential(pair[0], pair[1])
		require.Error(t, err)
	}
}

func TestFilesystemWorkerReaper(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root with CAP_SETUID/CAP_SETGID")
	}
	startReaperForTest(t)
	stop, err := StartFilesystemWorker(exec.Command("/bin/true"), 41001, 41000)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		stop()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("filesystem worker was not reaped in init mode")
	}
}
