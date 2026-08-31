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

//go:build linux && bwrap

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

func TestPrivateSessionPinsNamespacesBeforeMarkReady(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("namespace bind pin integration test requires root")
	}

	for _, uidMode := range []isolation.UidMode{
		isolation.UidModeSetpriv,
		isolation.UidModeUserns,
	} {
		t.Run(string(uidMode), func(t *testing.T) {
			testPrivateSessionPinsNamespacesBeforeMarkReady(t, uidMode)
		})
	}
}

func testPrivateSessionPinsNamespacesBeforeMarkReady(
	t *testing.T,
	uidMode isolation.UidMode,
) {
	t.Helper()

	cfg := isolation.Config{
		UpperRoot:       t.TempDir(),
		UpperMaxBytes:   1 << 30,
		AllowedWritable: []string{"/tmp"},
	}
	ctrl := NewController("", "")
	iso := isolation.NewBwrap(cfg)
	if !iso.Available() {
		t.Skip("bwrap lifecycle isolator is unavailable")
	}
	capabilities := iso.Capabilities()
	switch uidMode {
	case isolation.UidModeSetpriv:
		if !capabilities.SetprivAvailable {
			t.Skip("bwrap setpriv mode is unavailable")
		}
	case isolation.UidModeUserns:
		if !capabilities.UsernsAvailable {
			t.Skip("bwrap user namespace mode is unavailable")
		}
	default:
		t.Fatalf("unexpected uid mode %q", uidMode)
	}

	runner, err := NewIsolatedRunner(ctrl, iso, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runner.Close(); err != nil {
			t.Errorf("close isolated runner: %v", err)
		}
	})

	realPinner := runner.namespacePinner
	pinned := make(chan struct{})
	releasePinner := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releasePinner)
		})
	}
	t.Cleanup(release)

	var workload isolation.WorkloadIdentity
	var pins sessionNamespacePins
	runner.namespacePinner = func(
		ctx context.Context,
		identity isolation.WorkloadIdentity,
	) (sessionNamespacePins, error) {
		acquired, pinErr := realPinner(ctx, identity)
		if pinErr != nil {
			return acquired, pinErr
		}
		workload = identity
		pins = acquired
		close(pinned)

		select {
		case <-releasePinner:
			return acquired, nil
		case <-ctx.Done():
			_ = acquired.Close()
			return nil, ctx.Err()
		}
	}

	privateNetwork := false
	workspacePath := t.TempDir()
	createDone := make(chan struct{})
	var sessionID string
	var createErr error
	go func() {
		defer close(createDone)
		sessionID, createErr = runner.CreateIsolatedSession(
			&IsolatedSessionOptions{
				Profile:       string(isolation.ProfileStrict),
				WorkspacePath: workspacePath,
				WorkspaceMode: string(isolation.WorkspaceRW),
				ShareNet:      &privateNetwork,
				UidMode:       string(uidMode),
			},
		)
	}()

	pinTimer := time.NewTimer(15 * time.Second)
	defer pinTimer.Stop()
	select {
	case <-pinned:
	case <-createDone:
		t.Fatalf("session creation failed before namespace pin: %v", createErr)
	case <-pinTimer.C:
		t.Fatal("timed out waiting for namespace pins")
	}
	if pins == nil {
		t.Fatal("namespace pinner returned nil pins")
	}

	select {
	case <-createDone:
		t.Fatalf(
			"session creation completed before namespace pinner returned: %v",
			createErr,
		)
	default:
	}

	// Keep the pinner blocked long enough that any premature READY release
	// would have exec'd the persistent shell before this observation.
	time.Sleep(100 * time.Millisecond)
	processName, err := readProcessName(workload.PID)
	if err != nil {
		t.Fatalf("read blocked workload process name: %v", err)
	}
	if processName == filepath.Base(getShell()) {
		t.Fatalf(
			"workload process before pinner return = %q; shell executed before namespace pin completion",
			processName,
		)
	}

	assertPinnedNamespaceIdentity(t, workload, pins)

	directory := pins.Directory()
	netPath := pins.NetPath()
	userPath := pins.UserPath()
	release()
	waitForIntegrationEvent(t, createDone, "session creation")
	if createErr != nil {
		t.Fatalf("create private isolated session: %v", createErr)
	}
	if sessionID == "" {
		t.Fatal("create private isolated session returned an empty id")
	}

	if err := runner.DeleteIsolatedSession(sessionID); err != nil {
		t.Fatalf("delete private isolated session: %v", err)
	}
	for _, path := range []string{netPath, userPath, directory} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("namespace pin path %q after delete: %v", path, err)
		}
	}
}

func assertPinnedNamespaceIdentity(
	t *testing.T,
	workload isolation.WorkloadIdentity,
	pins sessionNamespacePins,
) {
	t.Helper()

	netFD := openNamespaceIntegration(t, pins.NetPath())
	defer unix.Close(netFD)
	userFD := openNamespaceIntegration(t, pins.UserPath())
	defer unix.Close(userFD)

	var netStat unix.Stat_t
	if err := unix.Fstat(netFD, &netStat); err != nil {
		t.Fatalf("stat pinned network namespace: %v", err)
	}
	if netStat.Ino != workload.NetNamespaceID {
		t.Fatalf(
			"pinned network namespace inode = %d, want workload inode %d",
			netStat.Ino,
			workload.NetNamespaceID,
		)
	}
	if namespaceType, err := unix.IoctlRetInt(
		netFD,
		unix.NS_GET_NSTYPE,
	); err != nil {
		t.Fatalf("get pinned network namespace type: %v", err)
	} else if namespaceType != unix.CLONE_NEWNET {
		t.Fatalf(
			"pinned network namespace type = %#x, want %#x",
			namespaceType,
			unix.CLONE_NEWNET,
		)
	}

	var userStat unix.Stat_t
	if err := unix.Fstat(userFD, &userStat); err != nil {
		t.Fatalf("stat pinned user namespace: %v", err)
	}
	if namespaceType, err := unix.IoctlRetInt(
		userFD,
		unix.NS_GET_NSTYPE,
	); err != nil {
		t.Fatalf("get pinned user namespace type: %v", err)
	} else if namespaceType != unix.CLONE_NEWUSER {
		t.Fatalf(
			"pinned user namespace type = %#x, want %#x",
			namespaceType,
			unix.CLONE_NEWUSER,
		)
	}

	ownerFD, err := unix.IoctlRetInt(netFD, unix.NS_GET_USERNS)
	if err != nil {
		t.Fatalf("get pinned network namespace owner: %v", err)
	}
	defer unix.Close(ownerFD)
	var ownerStat unix.Stat_t
	if err := unix.Fstat(ownerFD, &ownerStat); err != nil {
		t.Fatalf("stat pinned network namespace owner: %v", err)
	}
	if ownerStat.Dev != userStat.Dev || ownerStat.Ino != userStat.Ino {
		t.Fatalf(
			"pinned network namespace owner = %d:%d, user pin = %d:%d",
			ownerStat.Dev,
			ownerStat.Ino,
			userStat.Dev,
			userStat.Ino,
		)
	}

	identity := pins.Identity()
	if identity.PID != workload.PID ||
		identity.ProcessStartTimeTicks != workload.ProcessStartTimeTicks ||
		identity.NetDevice != uint64(netStat.Dev) ||
		identity.NetInode != netStat.Ino ||
		identity.OwningUserDevice != uint64(userStat.Dev) ||
		identity.OwningUserInode != userStat.Ino ||
		identity.OwningUserUID != uint32(os.Geteuid()) {
		t.Fatalf(
			"captured namespace identity = %+v, workload = %+v, net = %d:%d, user = %d:%d, owner uid = %d",
			identity,
			workload,
			netStat.Dev,
			netStat.Ino,
			userStat.Dev,
			userStat.Ino,
			os.Geteuid(),
		)
	}
}

func readProcessName(pid int) (string, error) {
	stat, err := os.ReadFile(
		filepath.Join("/proc", strconv.Itoa(pid), "stat"),
	)
	if err != nil {
		return "", err
	}
	text := string(stat)
	open := strings.IndexByte(text, '(')
	close := strings.LastIndex(text, ") ")
	if open < 0 || close <= open {
		return "", unix.EINVAL
	}
	return text[open+1 : close], nil
}

func openNamespaceIntegration(t *testing.T, path string) int {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("open namespace pin %q: %v", path, err)
	}
	return fd
}

func waitForIntegrationEvent(
	t *testing.T,
	event <-chan struct{},
	description string,
) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-event:
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", description)
	}
}
