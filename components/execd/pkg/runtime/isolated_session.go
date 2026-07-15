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
	"fmt"
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
	Binds              []isolation.BindMount
	ShareNet           *bool
	EnvPassthroughMode string
	EnvPassthroughKeys []string
	Uid                *uint32
	Gid                *uint32
	UidMode            string // "setpriv" (default) or "userns"
	IdleTimeoutSeconds int
}

// isolatedSession holds a long-running bash process inside a bwrap namespace.
type isolatedSession struct {
	id        string
	mu        sync.RWMutex
	runMu     sync.Mutex // serializes concurrent Run calls
	opts      *IsolatedSessionOptions
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	doneCh    chan struct{} // closed when the bwrap process exits
	upperID   string        // key in UpperManager, used for Release/Remove
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
		doneCh:    make(chan struct{}),
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
		Binds:         s.opts.Binds,
		ShareNet:      true,
	}

	switch s.opts.Profile {
	case string(isolation.ProfileBalanced):
		wrapOpts.Profile = isolation.ProfileBalanced
	case string(isolation.ProfileStrict), "":
		wrapOpts.Profile = isolation.ProfileStrict
	default:
		return fmt.Errorf("unknown isolation profile %q", s.opts.Profile)
	}

	wrapOpts.Workspace.Path = s.opts.WorkspacePath
	switch isolation.WorkspaceMode(s.opts.WorkspaceMode) {
	case isolation.WorkspaceRW:
		wrapOpts.Workspace.Mode = isolation.WorkspaceRW
	case isolation.WorkspaceRO:
		wrapOpts.Workspace.Mode = isolation.WorkspaceRO
	default:
		wrapOpts.Workspace.Mode = isolation.WorkspaceOverlay
	}

	if s.opts.ShareNet != nil {
		wrapOpts.ShareNet = *s.opts.ShareNet
	}
	if s.opts.EnvPassthroughMode != "" {
		wrapOpts.EnvPassthrough.Mode = isolation.EnvMode(s.opts.EnvPassthroughMode)
		wrapOpts.EnvPassthrough.Keys = s.opts.EnvPassthroughKeys
	} else {
		wrapOpts.EnvPassthrough.Mode = isolation.EnvModeDeny
	}
	wrapOpts.Uid = s.opts.Uid
	wrapOpts.Gid = s.opts.Gid
	if s.opts.UidMode != "" {
		wrapOpts.UidMode = isolation.UidMode(s.opts.UidMode)
	}
	wrapOpts.UpperDir = s.upperDir
	wrapOpts.WorkDir = s.workDir

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
		for _, f := range cmd.ExtraFiles {
			f.Close()
		}
		return err
	}

	for _, f := range cmd.ExtraFiles {
		f.Close()
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout

	go func() {
		_ = cmd.Wait()
		close(s.doneCh)
	}()

	// Brief startup check — if bwrap fails immediately (bad capabilities,
	// missing binary inside namespace, etc.) we detect it here instead of
	// waiting until the first Run call.
	select {
	case <-s.doneCh:
		return fmt.Errorf("bwrap process exited immediately after start")
	case <-time.After(100 * time.Millisecond):
	}

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
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		// Wait for the death-watch goroutine to finish cmd.Wait().
		<-s.doneCh
	}
	return nil
}

// dead returns true if the bwrap process has exited.
func (s *isolatedSession) dead() bool {
	select {
	case <-s.doneCh:
		return true
	default:
		return false
	}
}
