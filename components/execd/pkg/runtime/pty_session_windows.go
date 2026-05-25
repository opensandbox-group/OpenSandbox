// Copyright 2025 Alibaba Group Holding Ltd.
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

//go:build windows
// +build windows

package runtime

import (
	"errors"
	"io"
)

// ptySession is an opaque stub on Windows (real type in pty_session.go, !windows).
// All methods the controller layer calls must be present here so Windows cross-compilation succeeds.
type ptySession struct{}

// PTYSession is the public interface for an interactive PTY/pipe session.
// The concrete implementation (*ptySession) is unexported; callers outside
// this package must use this interface.
type PTYSession interface {
	LockWS() bool
	UnlockWS()
	IsRunning() bool
	IsPTY() bool
	ExitCode() int
	Done() <-chan struct{}
	StartPTY() error
	StartPipe() error
	WriteStdin(p []byte) (int, error)
	AttachOutput() (io.Reader, io.Reader, func())
	AttachOutputWithSnapshot(since int64) (io.Reader, io.Reader, func(), []byte, int64)
	SendSignal(name string)
	ResizePTY(cols, rows uint16) error
}

var errPTYSessionNotSupported = errors.New("pty session is not supported on windows")

// IsPTYSessionSupported reports whether PTY sessions are supported on this platform.
func IsPTYSessionSupported() bool { return false }

// NewPTYSessionID returns a new unique session ID (Windows stub).
func NewPTYSessionID() string { return "" }

// CreatePTYSession is not supported on Windows.
func (c *Controller) CreatePTYSession(id, cwd string) (PTYSession, error) { return nil, nil } //nolint:revive

// GetPTYSession is not supported on Windows.
func (c *Controller) GetPTYSession(id string) PTYSession { return nil } //nolint:revive

// DeletePTYSession is not supported on Windows.
func (c *Controller) DeletePTYSession(id string) error { //nolint:revive
	return errPTYSessionNotSupported
}

// GetPTYSessionStatus is not supported on Windows.
func (c *Controller) GetPTYSessionStatus(id string) (bool, int64, error) { //nolint:revive
	return false, 0, errPTYSessionNotSupported
}

// Method stubs so the controller layer can call them without build-tag guards.

func (s *ptySession) LockWS() bool                                 { return false }
func (s *ptySession) UnlockWS()                                    {}
func (s *ptySession) IsRunning() bool                              { return false }
func (s *ptySession) IsPTY() bool                                  { return false }
func (s *ptySession) ExitCode() int                                { return -1 }
func (s *ptySession) Done() <-chan struct{}                        { return nil }
func (s *ptySession) ReplayBuffer() *replayBuffer                  { return nil }
func (s *ptySession) StartPTY() error                              { return errPTYSessionNotSupported }
func (s *ptySession) StartPipe() error                             { return errPTYSessionNotSupported }
func (s *ptySession) WriteStdin(_ []byte) (int, error)             { return 0, errPTYSessionNotSupported }
func (s *ptySession) AttachOutput() (io.Reader, io.Reader, func()) { return nil, nil, func() {} }
func (s *ptySession) AttachOutputWithSnapshot(_ int64) (io.Reader, io.Reader, func(), []byte, int64) {
	return nil, nil, func() {}, nil, 0
}
func (s *ptySession) SendSignal(_ string)         {}
func (s *ptySession) ResizePTY(_, _ uint16) error { return nil }
