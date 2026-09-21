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

// Package revisionruntime composes the revision transaction foundation with
// one private mitmdump process session. It is not wired into a live profile.
package revisionruntime

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

const (
	processSessionReadyPoll          = 20 * time.Millisecond
	maxProcessSessionSocketPathBytes = 103
)

// ProcessSessionConfig describes one unused mitmdump receiver session. The
// parent remains caller-owned; the session creates and owns exactly one child
// directory beneath it and changes that directory to the target child UID/GID.
type ProcessSessionConfig struct {
	// ParentDir must remain at the same path and must not be group/world
	// writable for the complete child lifetime. Its mode must grant search to
	// the target UID/GID class so the child can reach its owned directory.
	ParentDir         string
	UID               int
	GID               int
	SubjectGeneration string
	MaxSnapshotBytes  int
}

// ProcessSession owns the local resources that bind one mitmdump process to
// one transport and coordinator. It is not connected to a running egress
// profile; the future launcher owner must stop the child before Close.
type ProcessSession struct {
	mu            sync.Mutex
	bootstrapMu   sync.Mutex
	bootstrap     bootstrapState
	parentPath    string
	parentRoot    *os.Root
	parentInfo    os.FileInfo
	targetUID     int
	targetGID     int
	directoryName string
	directoryInfo os.FileInfo
	launch        mitmproxy.RevisionIPCConfig
	transport     *revision.UnixTransport
	coordinator   *revision.Coordinator
	closed        bool
	cleaned       bool
}

func (*ProcessSession) String() string { return "revisionruntime.ProcessSession" }

func (*ProcessSession) GoString() string { return "revisionruntime.ProcessSession{}" }

// NewProcessSession provisions a fresh receiver directory, authentication
// token, control-plane generation, transport, and coordinator without dialing
// or launching mitmdump.
func NewProcessSession(cfg ProcessSessionConfig) (*ProcessSession, error) {
	if !validProcessSessionConfig(cfg) {
		return nil, revision.ErrInvalid
	}
	pathInfo, err := os.Lstat(cfg.ParentDir)
	if err != nil || !validProcessSessionParentInfo(pathInfo, cfg.UID, cfg.GID) {
		return nil, revision.ErrInvalid
	}
	parentRoot, err := os.OpenRoot(cfg.ParentDir)
	if err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	parentInfo, err := parentRoot.Stat(".")
	if err != nil || !os.SameFile(pathInfo, parentInfo) ||
		!validProcessSessionParentInfo(parentInfo, cfg.UID, cfg.GID) {
		_ = parentRoot.Close()
		return nil, revision.ErrInvalid
	}
	token, err := revision.NewSessionToken()
	if err != nil {
		_ = parentRoot.Close()
		return nil, revision.ErrTransportUnavailable
	}
	control, err := revision.NewSessionToken()
	if err != nil {
		_ = parentRoot.Close()
		return nil, revision.ErrTransportUnavailable
	}
	for control == token {
		control, err = revision.NewSessionToken()
		if err != nil {
			_ = parentRoot.Close()
			return nil, revision.ErrTransportUnavailable
		}
	}

	directoryName := ""
	created := false
	for attempt := 0; attempt < 8; attempt++ {
		directoryName = "revision-" + control[:16]
		err = parentRoot.Mkdir(directoryName, 0o700)
		if err == nil {
			created = true
			break
		}
		if !os.IsExist(err) {
			break
		}
		control, err = revision.NewSessionToken()
		if err != nil {
			break
		}
	}
	if !created {
		_ = parentRoot.Close()
		return nil, revision.ErrTransportUnavailable
	}
	owned := false
	defer func() {
		if !owned {
			_ = parentRoot.RemoveAll(directoryName)
			_ = parentRoot.Close()
		}
	}()
	if err := parentRoot.Chmod(directoryName, 0o700); err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	if err := parentRoot.Chown(directoryName, cfg.UID, cfg.GID); err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	directoryInfo, err := parentRoot.Lstat(directoryName)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 ||
		!processSessionOwnerMatches(directoryInfo, cfg.UID, cfg.GID) {
		return nil, revision.ErrTransportUnavailable
	}

	socketPath := filepath.Join(cfg.ParentDir, directoryName, "receiver.sock")
	if len([]byte(socketPath)) > maxProcessSessionSocketPathBytes {
		return nil, revision.ErrTransportUnavailable
	}
	launch := mitmproxy.RevisionIPCConfig{
		SocketPath:        socketPath,
		SessionToken:      token,
		ControlGeneration: control,
		SubjectGeneration: cfg.SubjectGeneration,
		MaxSnapshotBytes:  cfg.MaxSnapshotBytes,
	}
	transport, err := revision.NewUnixTransport(launch.SocketPath, launch.SessionToken, launch.MaxSnapshotBytes)
	if err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	coordinator, err := revision.New(launch.ControlGeneration, launch.SubjectGeneration, transport, launch.MaxSnapshotBytes)
	if err != nil {
		return nil, revision.ErrInvalid
	}
	owned = true
	return &ProcessSession{
		parentPath:    cfg.ParentDir,
		parentRoot:    parentRoot,
		parentInfo:    parentInfo,
		targetUID:     cfg.UID,
		targetGID:     cfg.GID,
		directoryName: directoryName,
		directoryInfo: directoryInfo,
		launch:        launch,
		transport:     transport,
		coordinator:   coordinator,
	}, nil
}

func validProcessSessionConfig(cfg ProcessSessionConfig) bool {
	return filepath.IsAbs(cfg.ParentDir) && strings.IndexByte(cfg.ParentDir, 0) < 0 &&
		validProcessSessionID(cfg.UID) && validProcessSessionID(cfg.GID) &&
		validProcessSessionGeneration(cfg.SubjectGeneration) &&
		cfg.MaxSnapshotBytes > 0
}

func validProcessSessionID(value int) bool {
	return value >= 0 && uint64(value) < uint64(math.MaxUint32)
}

func validProcessSessionParentInfo(info os.FileInfo, uid, gid int) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0 &&
		processSessionCanTraverse(info, uid, gid)
}

func processSessionCanTraverse(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	mode := info.Mode().Perm()
	switch {
	case uint64(stat.Uid) == uint64(uid):
		return mode&0o100 != 0
	case uint64(stat.Gid) == uint64(gid):
		return mode&0o010 != 0
	default:
		return mode&0o001 != 0
	}
}

func processSessionOwnerMatches(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(uid) && uint64(stat.Gid) == uint64(gid)
}

func validProcessSessionGeneration(value string) bool {
	return value != "" && strings.IndexByte(value, 0) < 0 && utf8.ValidString(value) &&
		utf8.RuneCountInString(value) <= 128
}

// MitmproxyConfig returns an owned copy of the child handoff bundle.
func (s *ProcessSession) MitmproxyConfig() (*mitmproxy.RevisionIPCConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, revision.ErrClosed
	}
	if !s.parentPathMatches() {
		return nil, revision.ErrTransportUnavailable
	}
	copy := s.launch
	return &copy, nil
}

// Coordinator returns the session coordinator. ProcessSession retains
// ownership and closes it when the session closes.
func (s *ProcessSession) Coordinator() (*revision.Coordinator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, revision.ErrClosed
	}
	return s.coordinator, nil
}

// WaitReady waits for the authenticated endpoint and accepts only a fresh
// receiver with no installed revision. A reused or foreign receiver is never
// treated as a successful process launch.
func (s *ProcessSession) WaitReady(ctx context.Context) error {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return revision.ErrClosed
		}
		if !s.parentPathMatches() {
			s.mu.Unlock()
			return revision.ErrTransportUnavailable
		}
		transport := s.transport
		s.mu.Unlock()

		active, err := transport.Readback(ctx)
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return revision.ErrClosed
		}
		if err == nil {
			if active != nil {
				return revision.ErrIndeterminate
			}
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		timer := time.NewTimer(processSessionReadyPoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *ProcessSession) parentPathMatches() bool {
	current, err := os.Lstat(s.parentPath)
	return err == nil && os.SameFile(current, s.parentInfo) &&
		validProcessSessionParentInfo(current, s.targetUID, s.targetGID)
}

// Close fences local coordination and removes only the directory identity
// created by this session. A replacement path is left untouched.
func (s *ProcessSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.coordinator.Close()
	}
	if s.cleaned {
		return nil
	}
	current, err := s.parentRoot.Lstat(s.directoryName)
	if os.IsNotExist(err) {
		s.cleaned = true
		if err := s.parentRoot.Close(); err != nil {
			return revision.ErrTransportUnavailable
		}
		return nil
	}
	if err != nil || !os.SameFile(current, s.directoryInfo) {
		_ = s.parentRoot.Close()
		s.cleaned = true
		return revision.ErrTransportUnavailable
	}
	if err := s.parentRoot.RemoveAll(s.directoryName); err != nil {
		return revision.ErrTransportUnavailable
	}
	s.cleaned = true
	if err := s.parentRoot.Close(); err != nil {
		return revision.ErrTransportUnavailable
	}
	return nil
}
