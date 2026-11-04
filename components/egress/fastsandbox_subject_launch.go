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

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
)

// No existing Fast Sandbox subject capacity limit exists. This bounds the
// installation-only owner, independently of the policy subject registry.
const fastSandboxRevisionMaxSubjects = 4096
const fastSandboxRevisionRetainedBudget int64 = 2 * fastSandboxRevisionMaxSubjects * defaultRevisionMaxSnapshotSize

type fastSandboxSubjectSessionConfig struct {
	parent   string
	uid, gid int
}

// This is a shared-subject process session, not a sidecar ProcessSession. Its
// caller stops/reaps the exact child before closing the private directory.
type fastSandboxSubjectSession struct {
	root                      *os.Root
	parent, directory         string
	parentInfo, directoryInfo os.FileInfo
	cfg                       fastSandboxSubjectSessionConfig
	handoff                   mitmproxy.SubjectRevisionIPCConfig
	client                    *revision.SubjectLifecycleClient // reserved for the B1b drain worker
}

func newFastSandboxSubjectSession(cfg fastSandboxSubjectSessionConfig) (*fastSandboxSubjectSession, error) {
	if !filepath.IsAbs(cfg.parent) || cfg.uid < 0 || cfg.gid < 0 ||
		uint64(cfg.uid) >= 1<<32-1 || uint64(cfg.gid) >= 1<<32-1 {
		return nil, revision.ErrInvalid
	}
	info, err := os.Lstat(cfg.parent)
	if err != nil || !subjectSessionParent(info, cfg) {
		return nil, revision.ErrInvalid
	}
	root, err := os.OpenRoot(cfg.parent)
	if err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, revision.ErrInvalid
	}
	s := &fastSandboxSubjectSession{root: root, cfg: cfg, parent: cfg.parent, parentInfo: info}
	owned := false
	defer func() {
		if !owned {
			if s.directory != "" {
				_ = root.RemoveAll(s.directory)
			}
			_ = root.Close()
		}
	}()
	token, err := revision.NewSessionToken()
	if err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	control, err := revision.NewSessionToken()
	if err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	for control == token {
		control, err = revision.NewSessionToken()
		if err != nil {
			return nil, revision.ErrTransportUnavailable
		}
	}
	// A collision fails the attempt; retrying the launch creates a new session.
	directory := "subject-" + control[:16]
	if err := root.Mkdir(directory, 0o700); err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	s.directory = directory
	if err := root.Chown(directory, cfg.uid, cfg.gid); err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	s.directoryInfo, err = root.Lstat(directory)
	if err != nil || !s.directoryInfo.IsDir() || s.directoryInfo.Mode().Perm() != 0o700 ||
		!subjectSessionOwner(s.directoryInfo, cfg.uid, cfg.gid) {
		return nil, revision.ErrTransportUnavailable
	}
	s.handoff = mitmproxy.SubjectRevisionIPCConfig{
		SocketPath:   filepath.Join(cfg.parent, directory, "receiver.sock"),
		SessionToken: token, ControlGeneration: control,
		MaxSubjects: fastSandboxRevisionMaxSubjects, MaxSnapshotBytes: defaultRevisionMaxSnapshotSize,
		RetainedPayloadBudget: fastSandboxRevisionRetainedBudget,
	}
	if _, err := mitmproxy.WithSubjectRevision(mitmproxy.Config{}, s.handoff); err != nil {
		return nil, revision.ErrInvalid
	}
	s.client, err = revision.NewSubjectLifecycleClient(s.handoff.SocketPath, token, control)
	if err != nil {
		return nil, err
	}
	owned = true
	return s, nil
}

func subjectSessionOwner(info os.FileInfo, uid, gid int) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Uid) == uint64(uid) && uint64(stat.Gid) == uint64(gid)
}

func subjectSessionParent(info os.FileInfo, cfg fastSandboxSubjectSessionConfig) bool {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	mode := info.Mode().Perm()
	switch {
	case uint64(stat.Uid) == uint64(cfg.uid):
		return mode&0o100 != 0
	case uint64(stat.Gid) == uint64(cfg.gid):
		return mode&0o010 != 0
	default:
		return mode&0o001 != 0
	}
}

func (s *fastSandboxSubjectSession) parentMatches() bool {
	info, err := os.Lstat(s.parent)
	return err == nil && os.SameFile(info, s.parentInfo) && subjectSessionParent(info, s.cfg)
}

func (s *fastSandboxSubjectSession) waitReady(ctx context.Context) error {
	for {
		if !s.parentMatches() {
			return revision.ErrTransportUnavailable
		}
		err := revision.CheckFreshSubjectSession(ctx, s.handoff.SocketPath, s.handoff.SessionToken, s.handoff.ControlGeneration)
		if !errors.Is(err, revision.ErrTransportUnavailable) {
			return err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// close is serialized by the launch owner after detaching the exact child.
// A replaced directory is never removed, even if its name is reused.
func (s *fastSandboxSubjectSession) close() error {
	defer s.root.Close()
	info, err := s.root.Lstat(s.directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !os.SameFile(info, s.directoryInfo) {
		return revision.ErrTransportUnavailable
	}
	if err := s.root.RemoveAll(s.directory); err != nil {
		return revision.ErrTransportUnavailable
	}
	return nil
}

type fastSandboxSubjectLaunchOwner struct {
	cfg      fastSandboxSubjectSessionConfig
	mu       sync.Mutex
	sessions map[*mitmproxy.Running]*fastSandboxSubjectSession
	stop     func(*mitmproxy.Running)
}

func newFastSandboxSubjectLaunchOwner(enabled bool, uid, gid int) (*fastSandboxSubjectLaunchOwner, error) {
	if !enabled {
		return nil, nil
	}
	if err := os.MkdirAll(defaultRevisionSessionParent, 0o711); err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	// The same parent is used by sidecar sessions; retain its established mode.
	if err := os.Chmod(defaultRevisionSessionParent, 0o711); err != nil {
		return nil, revision.ErrTransportUnavailable
	}
	return &fastSandboxSubjectLaunchOwner{
		cfg:      fastSandboxSubjectSessionConfig{parent: defaultRevisionSessionParent, uid: uid, gid: gid},
		sessions: make(map[*mitmproxy.Running]*fastSandboxSubjectSession),
		stop:     func(r *mitmproxy.Running) { mitmproxy.GracefulShutdown(r, time.Second) },
	}, nil
}

func (o *fastSandboxSubjectLaunchOwner) launch(ctx context.Context, cfg mitmproxy.Config, launch func(mitmproxy.Config) (*mitmproxy.Running, error)) (*mitmproxy.Running, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.RevisionIPC != nil || cfg.SubjectRevisionIPC != nil {
		return nil, revision.ErrInvalid
	}
	session, err := newFastSandboxSubjectSession(o.cfg)
	if err != nil {
		return nil, err
	}
	child, err := mitmproxy.WithSubjectRevision(cfg, session.handoff)
	if err != nil {
		_ = session.close()
		return nil, err
	}
	running, err := launch(child)
	if err != nil || running == nil {
		if cleanupErr := session.close(); cleanupErr != nil {
			return nil, cleanupErr
		}
		if err != nil {
			return nil, err
		}
		return nil, revision.ErrTransportUnavailable
	}
	if err := session.waitReady(ctx); err != nil {
		o.stop(running)
		if cleanupErr := session.close(); cleanupErr != nil {
			return nil, cleanupErr
		}
		return nil, fmt.Errorf("subject receiver startup: %w", err)
	}
	o.mu.Lock()
	o.sessions[running] = session
	o.mu.Unlock()
	return running, nil
}

func (o *fastSandboxSubjectLaunchOwner) close(running *mitmproxy.Running) error {
	if o == nil || running == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	session := o.sessions[running]
	if session == nil {
		return nil
	}
	delete(o.sessions, running)
	return session.close()
}
