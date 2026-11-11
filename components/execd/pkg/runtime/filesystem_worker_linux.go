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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strconv"
	"syscall"
)

// filesystemCredential resolves the target account's supplementary groups.
// Unlike a best-effort lookup, an NSS/group failure must never grant the
// daemon's groups or silently select a different primary group.
func filesystemCredential(uid, gid uint32) (*syscall.Credential, string, error) {
	if uid == ^uint32(0) || gid == ^uint32(0) {
		return nil, "", fmt.Errorf("filesystem uid/gid must be at most 4294967294")
	}
	cred := &syscall.Credential{Uid: uid, Gid: gid}
	home := "/"
	account, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		var unknown user.UnknownUserIdError
		if !errors.As(err, &unknown) {
			return nil, "", fmt.Errorf("lookup filesystem user: %w", err)
		}
	} else {
		home = account.HomeDir
		groups, err := account.GroupIds()
		if err != nil {
			return nil, "", fmt.Errorf("lookup filesystem groups: %w", err)
		}
		for _, group := range groups {
			id, err := strconv.ParseUint(group, 10, 32)
			if err != nil || id == 1<<32-1 {
				return nil, "", fmt.Errorf("invalid supplementary group %q", group)
			}
			cred.Groups = append(cred.Groups, uint32(id))
		}
	}
	// Avoid privileged setgroups calls only when the full identity already
	// matches, including supplementary groups and real/effective IDs.
	if uid == uint32(os.Getuid()) && uid == uint32(os.Geteuid()) &&
		gid == uint32(os.Getgid()) && gid == uint32(os.Getegid()) {
		current, err := os.Getgroups()
		if err != nil {
			return nil, "", err
		}
		groups := make([]uint32, len(current))
		for i, group := range current {
			groups[i] = uint32(group)
		}
		slices.Sort(groups)
		slices.Sort(cred.Groups)
		if slices.Equal(groups, cred.Groups) {
			return nil, home, nil
		}
	}
	return cred, home, nil
}

// StartFilesystemWorker launches trusted file handlers with request-specific
// credentials, registering with the init-mode reaper. The returned function
// cancels and reaps the child; callers must invoke it on every exit path.
func StartFilesystemWorker(cmd *exec.Cmd, uid, gid uint32) (func(), error) {
	cred, home, err := filesystemCredential(uid, gid)
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	// Do not expose allocation credentials or the daemon's environment to the
	// reduced-identity process. Relative paths are rooted at /; ~ uses its home.
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "TMPDIR=/tmp"}
	// These are trusted file handlers, not a user command. Their boundary is
	// Linux DAC, just as for the ordinary file API; no shell is executed.
	process, err := launchManaged(cmd, withoutHardening())
	if err != nil {
		return nil, credentialStartHint(err, cred)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = process.Wait()
	}()
	return func() {
		process.Cancel(func() { _ = cmd.Process.Kill() })
		<-done
	}, nil
}
