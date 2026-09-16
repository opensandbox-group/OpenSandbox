//go:build !windows
// +build !windows

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

package runtime

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCredential_NilIdentityReturnsNil(t *testing.T) {
	cred, err := buildCredential(nil, nil)
	require.NoError(t, err)
	assert.Nil(t, cred)
}

// Explicit ids matching the identity execd already runs as must stay on the
// plain exec path: a non-nil Credential makes the child call setgroups even
// when every id matches, and setgroups requires CAP_SETGID regardless of the
// requested values (#1802).
func TestBuildCredential_SameIdentityReturnsNil(t *testing.T) {
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())

	cred, err := buildCredential(&uid, &gid)
	require.NoError(t, err)
	assert.Nil(t, cred, "explicit current uid+gid must not produce a credential")

	cred, err = buildCredential(nil, &gid)
	require.NoError(t, err)
	assert.Nil(t, cred, "explicit current gid must not produce a credential")

	// uid-only: the switch is skipped only when the user entry resolves to
	// the daemon's own groups; otherwise the credential machinery still runs
	// (the request asks for that user's primary GID and supplemental groups).
	cred, err = buildCredential(&uid, nil)
	require.NoError(t, err)
	if sameProcessGroups(uid) {
		assert.Nil(t, cred, "uid-only current identity with matching groups must not produce a credential")
	} else {
		require.NotNil(t, cred)
		assert.Equal(t, uid, cred.Uid)
	}
}

func TestSameProcessGroupsCurrentUID(t *testing.T) {
	// The daemon's own uid must resolve to its own primary GID in any sane
	// environment (root container: root/0/0; dev laptop: the logged-in user).
	assert.True(t, sameProcessGroups(uint32(os.Getuid())))

	// An unknown uid never matches.
	assert.False(t, sameProcessGroups(4294967294))
}

func TestBuildCredential_IdentitySwitchBuildsCredential(t *testing.T) {
	otherUID := uint32(4294967294) // max-1; not a real login uid in practice
	if otherUID == uint32(os.Getuid()) {
		otherUID-- // paranoia: never collide with the real current uid
	}
	otherGID := otherUID - 1

	cred, err := buildCredential(&otherUID, &otherGID)
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, otherUID, cred.Uid)
	assert.Equal(t, otherGID, cred.Gid)

	// uid only: credential is built even if the user entry is unknown
	cred, err = buildCredential(&otherUID, nil)
	require.NoError(t, err)
	require.NotNil(t, cred)
	assert.Equal(t, otherUID, cred.Uid)
}

func TestCredentialStartHint(t *testing.T) {
	cred := &syscall.Credential{Uid: 1000, Gid: 1000}
	permErr := &os.PathError{Op: "fork/exec", Path: "/usr/bin/bash", Err: syscall.EPERM}

	hinted := credentialStartHint(permErr, cred)
	require.ErrorIs(t, hinted, os.ErrPermission)
	assert.Contains(t, hinted.Error(), "CAP_SETUID")
	assert.Contains(t, hinted.Error(), "drop_capabilities")
	assert.Contains(t, hinted.Error(), "uid=1000")

	same := credentialStartHint(permErr, nil)
	assert.Equal(t, permErr, same)

	otherErr := &os.PathError{Op: "fork/exec", Path: "/usr/bin/bash", Err: syscall.ENOENT}
	assert.Equal(t, otherErr, credentialStartHint(otherErr, cred))

	assert.NoError(t, credentialStartHint(nil, cred))
	assert.True(t, errors.Is(hinted, syscall.EPERM))
}
