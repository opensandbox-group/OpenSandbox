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

//go:build !windows

package runtime

import (
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

// IsolatedSessionOptions bundles the parameters for creating an isolated session.
type IsolatedSessionOptions struct {
	Profile            string
	WorkspacePath      string
	WorkspaceMode      string
	ExtraWritable      []string
	ShareNet           *bool
	EnvPassthroughMode string
	EnvPassthroughKeys []string
	Uid                *uint32
	Gid                *uint32
	IdleTimeoutSeconds int
}

// isolatedSession holds a long-running bash process inside a bwrap namespace.
type isolatedSession struct {
	id        string
	mu        sync.RWMutex
	opts      *IsolatedSessionOptions
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	upperDir  string
	workDir   string
	createdAt time.Time
	lastRunAt time.Time
	isolator  isolation.Isolator
}

func newIsolatedSession(id string, opts *IsolatedSessionOptions, iso isolation.Isolator) *isolatedSession {
	return &isolatedSession{
		id:        id,
		opts:      opts,
		isolator:  iso,
		createdAt: time.Now(),
		lastRunAt: time.Now(),
	}
}

// start launches bwrap + bash inside a namespace.
func (s *isolatedSession) start() error {
	cmd := exec.Command("bash", "--noprofile", "--norc")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	wrapOpts := isolation.WrapOptions{
		ExtraWritable: s.opts.ExtraWritable,
		ShareNet:      true,
	}

	switch s.opts.Profile {
	case "balanced":
		wrapOpts.Profile = isolation.ProfileBalanced
	default:
		wrapOpts.Profile = isolation.ProfileStrict
	}

	wrapOpts.Workspace.Path = s.opts.WorkspacePath
	switch s.opts.WorkspaceMode {
	case "rw":
		wrapOpts.Workspace.Mode = isolation.WorkspaceRW
	case "ro":
		wrapOpts.Workspace.Mode = isolation.WorkspaceRO
	default:
		wrapOpts.Workspace.Mode = isolation.WorkspaceOverlay
	}

	if s.opts.ShareNet != nil {
		wrapOpts.ShareNet = *s.opts.ShareNet
	}
	wrapOpts.EnvPassthrough.Mode = isolation.EnvMode(s.opts.EnvPassthroughMode)
	wrapOpts.EnvPassthrough.Keys = s.opts.EnvPassthroughKeys
	wrapOpts.Uid = s.opts.Uid
	wrapOpts.Gid = s.opts.Gid

	if err := s.isolator.Wrap(cmd, wrapOpts); err != nil {
		return err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return err
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	return nil
}

// stop kills the bwrap process group and waits for process reaping.
func (s *isolatedSession) stop() error {
	if s.stdin != nil {
		s.stdin.Close()
	}
	if s.stdout != nil {
		s.stdout.Close()
	}
	if s.cmd != nil && s.cmd.Process != nil {
		if err := syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return err
		}
		// Wait reaps the zombie, releases kernel resources (namespaces, fds).
		if _, err := s.cmd.Process.Wait(); err != nil {
			return err
		}
	}
	return nil
}
