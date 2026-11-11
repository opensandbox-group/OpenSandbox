//go:build linux

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

package quarantine

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestNamespaceTargetRealPinAndClose(t *testing.T) {
	target, err := PinNamespace()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	require.NotNil(t, target.file)
	fd := int(target.file.Fd())
	flags, err := unix.FcntlInt(target.file.Fd(), unix.F_GETFD, 0)
	require.NoError(t, err)
	require.NotZero(t, flags&unix.FD_CLOEXEC, "namespace pin must not leak into child processes")
	require.NoError(t, target.Check())
	another, err := PinNamespace()
	require.NoError(t, err)
	require.Equal(t, target.identity, another.identity)
	require.NoError(t, another.Close())
	require.NoError(t, target.Check(), "closing another descriptor must not release this pin")
	require.NoError(t, target.Close())
	require.NoError(t, target.Close())
	require.ErrorContains(t, target.Check(), "closed")
	var stat unix.Stat_t
	require.ErrorIs(t, unix.Fstat(fd, &stat), unix.EBADF)
}

func TestNamespaceTargetRejectsInvalidOrChangedPin(t *testing.T) {
	for _, field := range []string{"device", "inode"} {
		t.Run(field, func(t *testing.T) {
			target, err := PinNamespace()
			require.NoError(t, err)
			defer target.Close()
			if field == "device" {
				target.identity.dev++
			} else {
				target.identity.inode++
			}
			require.ErrorContains(t, target.Check(), "identity changed")
		})
	}
	file, err := os.CreateTemp(t.TempDir(), "not-nsfs")
	require.NoError(t, err)
	defer file.Close()
	_, err = readNetworkNamespaceIdentity(file)
	require.ErrorContains(t, err, "not nsfs")
	wrong, err := os.Open("/proc/self/ns/mnt")
	require.NoError(t, err)
	defer wrong.Close()
	_, err = readNetworkNamespaceIdentity(wrong)
	require.ErrorContains(t, err, "not a network namespace")
	target, err := PinNamespace()
	require.NoError(t, err)
	require.NoError(t, target.file.Close())
	require.Error(t, target.Check(), "closed descriptor must not be treated as a valid identity")
	require.Error(t, target.Close(), "an uncertain close is reported")
	require.NoError(t, target.Close())
	var missing *NamespaceTarget
	require.Error(t, missing.Check())
	require.NoError(t, missing.Close())
	require.Error(t, (&NamespaceTarget{}).Check())
}

func TestNamespaceTargetConcurrentCheckAndClose(t *testing.T) {
	target, err := PinNamespace()
	require.NoError(t, err)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = target.Check()
			}
		}()
	}
	require.NoError(t, target.Close())
	wg.Wait()
	require.Error(t, target.Check())
	require.NoError(t, target.Close())
}
