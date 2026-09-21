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

package revisionruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/revision"
	"github.com/stretchr/testify/require"
)

func processSessionConfig(parent string) ProcessSessionConfig {
	return ProcessSessionConfig{
		ParentDir:         parent,
		UID:               os.Getuid(),
		GID:               os.Getgid(),
		SubjectGeneration: "subject-a",
		MaxSnapshotBytes:  4096,
	}
}

func processSessionParent(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("/tmp", "osps-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(parent)) })
	return parent
}

func TestProcessSessionProvisioningAndCleanup(t *testing.T) {
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)

	launch, err := session.MitmproxyConfig()
	require.NoError(t, err)
	require.Equal(t, "subject-a", launch.SubjectGeneration)
	require.NotEmpty(t, launch.ControlGeneration)
	require.NotEqual(t, launch.ControlGeneration, launch.SessionToken)
	require.Len(t, launch.SessionToken, 43)
	require.Equal(t, 4096, launch.MaxSnapshotBytes)

	directory := filepath.Dir(launch.SocketPath)
	info, err := os.Lstat(directory)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Equal(t, uint32(os.Getuid()), stat.Uid)
	require.Equal(t, uint32(os.Getgid()), stat.Gid)

	coordinator, err := session.Coordinator()
	require.NoError(t, err)
	confirmed, err := coordinator.Confirmed()
	require.NoError(t, err)
	require.Nil(t, confirmed)

	formatted := fmt.Sprintf("%v %#v", session, session)
	require.NotContains(t, formatted, launch.SessionToken)
	require.NotContains(t, formatted, launch.SocketPath)

	require.NoError(t, session.Close())
	require.NoDirExists(t, directory)
	_, err = session.MitmproxyConfig()
	require.ErrorIs(t, err, revision.ErrClosed)
	_, err = session.Coordinator()
	require.ErrorIs(t, err, revision.ErrClosed)
	_, err = coordinator.Confirmed()
	require.ErrorIs(t, err, revision.ErrClosed)
	require.NoError(t, session.Close())
}

func TestProcessSessionUsesFreshSessionIdentities(t *testing.T) {
	parent := processSessionParent(t)
	first, err := NewProcessSession(processSessionConfig(parent))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	second, err := NewProcessSession(processSessionConfig(parent))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	firstConfig, err := first.MitmproxyConfig()
	require.NoError(t, err)
	secondConfig, err := second.MitmproxyConfig()
	require.NoError(t, err)
	require.NotEqual(t, firstConfig.SocketPath, secondConfig.SocketPath)
	require.NotEqual(t, firstConfig.SessionToken, secondConfig.SessionToken)
	require.NotEqual(t, firstConfig.ControlGeneration, secondConfig.ControlGeneration)
}

func TestProcessSessionRejectsOverlongSocketPathAndCleansDirectory(t *testing.T) {
	parent := filepath.Join(processSessionParent(t), strings.Repeat("a", 80))
	require.NoError(t, os.Mkdir(parent, 0o700))
	session, err := NewProcessSession(processSessionConfig(parent))
	require.Nil(t, session)
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestProcessSessionRejectsInvalidConfigWithoutFilesystemChanges(t *testing.T) {
	parent := processSessionParent(t)
	valid := processSessionConfig(parent)
	cases := []ProcessSessionConfig{
		{},
		func() ProcessSessionConfig { value := valid; value.ParentDir = "relative"; return value }(),
		func() ProcessSessionConfig { value := valid; value.UID = -1; return value }(),
		func() ProcessSessionConfig { value := valid; value.GID = -1; return value }(),
		func() ProcessSessionConfig { value := valid; value.SubjectGeneration = ""; return value }(),
		func() ProcessSessionConfig { value := valid; value.SubjectGeneration = "subject\x00a"; return value }(),
		func() ProcessSessionConfig { value := valid; value.MaxSnapshotBytes = 0; return value }(),
	}
	if strconv.IntSize > 32 {
		tooLarge := int64(math.MaxUint32)
		uid := valid
		uid.UID = int(tooLarge)
		gid := valid
		gid.GID = int(tooLarge)
		cases = append(cases, uid, gid)
	}
	for _, candidate := range cases {
		withBefore, err := os.ReadDir(parent)
		require.NoError(t, err)
		session, err := NewProcessSession(candidate)
		require.Nil(t, session)
		require.ErrorIs(t, err, revision.ErrInvalid)
		withAfter, readErr := os.ReadDir(parent)
		require.NoError(t, readErr)
		require.Equal(t, withBefore, withAfter)
	}
}

func TestProcessSessionDetectsParentPathReplacementAndCleansAnchoredRoot(t *testing.T) {
	parent := processSessionParent(t)
	session, err := NewProcessSession(processSessionConfig(parent))
	require.NoError(t, err)
	renamed := parent + ".owned"
	require.NoError(t, os.Rename(parent, renamed))
	require.NoError(t, os.Mkdir(parent, 0o700))
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(parent))
		require.NoError(t, os.RemoveAll(renamed))
	})

	config, err := session.MitmproxyConfig()
	require.Nil(t, config)
	require.ErrorIs(t, err, revision.ErrTransportUnavailable)
	require.NoError(t, session.Close())
	entries, readErr := os.ReadDir(renamed)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestProcessSessionRejectsWritableOrSymlinkParent(t *testing.T) {
	writable := processSessionParent(t)
	require.NoError(t, os.Chmod(writable, 0o770))
	session, err := NewProcessSession(processSessionConfig(writable))
	require.Nil(t, session)
	require.ErrorIs(t, err, revision.ErrInvalid)
	entries, readErr := os.ReadDir(writable)
	require.NoError(t, readErr)
	require.Empty(t, entries)

	target := processSessionParent(t)
	container := processSessionParent(t)
	linked := filepath.Join(container, "linked")
	require.NoError(t, os.Symlink(target, linked))
	session, err = NewProcessSession(processSessionConfig(linked))
	require.Nil(t, session)
	require.ErrorIs(t, err, revision.ErrInvalid)
}

func TestProcessSessionRejectsParentWithoutTargetTraversal(t *testing.T) {
	parent := processSessionParent(t)
	config := processSessionConfig(parent)
	config.UID++
	config.GID++
	session, err := NewProcessSession(config)
	require.Nil(t, session)
	require.ErrorIs(t, err, revision.ErrInvalid)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestProcessSessionParentTraversalUsesTargetIdentityClass(t *testing.T) {
	parent := processSessionParent(t)
	info := func(mode os.FileMode) os.FileInfo {
		require.NoError(t, os.Chmod(parent, mode))
		value, err := os.Lstat(parent)
		require.NoError(t, err)
		return value
	}
	owner := info(0o700).Sys().(*syscall.Stat_t)
	uid, gid := int(owner.Uid), int(owner.Gid)
	require.True(t, processSessionCanTraverse(info(0o700), uid, gid))
	require.True(t, processSessionCanTraverse(info(0o710), uid+1, gid))
	require.True(t, processSessionCanTraverse(info(0o701), uid+1, gid+1))
	require.False(t, processSessionCanTraverse(info(0o700), uid+1, gid+1))
}

func TestProcessSessionWaitReadyRequiresFreshEmptyReceiver(t *testing.T) {
	tests := []struct {
		name     string
		response func(string, string) any
		want     error
	}{
		{
			name: "fresh receiver",
			response: func(_, _ string) any {
				return map[string]any{"revision": nil}
			},
		},
		{
			name: "unexpected active revision",
			response: func(control, subject string) any {
				return map[string]any{"revision": revision.Identity{
					ControlGeneration: control,
					SubjectGeneration: subject,
					DecisionEpoch:     1,
					VaultRevision:     0,
					PolicyEpoch:       0,
					Digest:            strings.Repeat("0", 64),
				}}
			},
			want: revision.ErrIndeterminate,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, session.Close()) })
			config, err := session.MitmproxyConfig()
			require.NoError(t, err)
			serveProcessSessionReadback(t, session, config.SessionToken, test.response)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err = session.WaitReady(ctx)
			if test.want == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, test.want)
			}
		})
	}
}

func TestProcessSessionWaitReadyHonorsContext(t *testing.T) {
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, session.WaitReady(ctx), context.DeadlineExceeded)
}

func TestProcessSessionCloseFencesSuccessfulReadinessInFlight(t *testing.T) {
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	config, err := session.MitmproxyConfig()
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	serveProcessSessionReadback(t, session, config.SessionToken, func(_, _ string) any {
		close(started)
		<-release
		return map[string]any{"revision": nil}
	})

	result := make(chan error, 1)
	go func() { result <- session.WaitReady(context.Background()) }()
	<-started
	require.NoError(t, session.Close())
	close(release)
	require.ErrorIs(t, <-result, revision.ErrClosed)
}

func TestProcessSessionCleanupRefusesReplacementDirectory(t *testing.T) {
	session, err := NewProcessSession(processSessionConfig(processSessionParent(t)))
	require.NoError(t, err)
	config, err := session.MitmproxyConfig()
	require.NoError(t, err)
	directory := filepath.Dir(config.SocketPath)
	original := directory + ".owned"
	require.NoError(t, os.Rename(directory, original))
	require.NoError(t, os.Mkdir(directory, 0o700))
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(directory))
		require.NoError(t, os.RemoveAll(original))
	})

	require.ErrorIs(t, session.Close(), revision.ErrTransportUnavailable)
	require.DirExists(t, directory)
	coordinator, err := session.Coordinator()
	require.Nil(t, coordinator)
	require.ErrorIs(t, err, revision.ErrClosed)
}

func serveProcessSessionReadback(
	t *testing.T,
	session *ProcessSession,
	token string,
	response func(string, string) any,
) *http.Server {
	t.Helper()
	config, err := session.MitmproxyConfig()
	require.NoError(t, err)
	listener, err := net.Listen("unix", config.SocketPath)
	require.NoError(t, err)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/revisions/active" ||
			r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response(config.ControlGeneration, config.SubjectGeneration))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})
	return server
}
