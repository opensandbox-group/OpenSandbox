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

//go:build !windows

package runtime

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type operationBlockingClose struct {
	entered chan struct{}
	release chan struct{}
}

func (c *operationBlockingClose) Write(p []byte) (int, error) { return len(p), nil }
func (c *operationBlockingClose) Close() error {
	close(c.entered)
	<-c.release
	return nil
}

func TestOperationCleanupDoesNotBlockUnrelatedRecovery(t *testing.T) {
	for _, slowClose := range []bool{false, true} {
		t.Run(fmt.Sprintf("slowClose=%v", slowClose), func(t *testing.T) {
			c := NewController("", "")
			c.initOperations()
			now := time.Unix(1700000000, 0)
			c.operations.now = func() time.Time { return now }
			key := testOperationID(c, "old-pty-session")
			_, err := c.CreatePTYOperation("owner", key, "", "")
			require.NoError(t, err)
			old := awaitOperationCreated(t, c, "pty", key)
			now = now.Add(23 * time.Hour)
			freshKey := testOperationID(c, "fresh-command")
			fresh, _, err := c.claimOperation("owner", "command", freshKey, "same")
			require.NoError(t, err)
			c.finishCreation(fresh, false)
			now = now.Add(time.Hour + time.Second)
			session := c.getPTYSession(old.ID)
			var cleanupDone chan struct{}
			if slowClose {
				closer := &operationBlockingClose{make(chan struct{}), make(chan struct{})}
				session.stdin = closer
				cleanupDone = make(chan struct{})
				go func() { c.cleanupOperations(); close(cleanupDone) }()
				<-closer.entered
				defer func() { close(closer.release); <-cleanupDone }()
			} else {
				session.mu.Lock()
				defer session.mu.Unlock()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if slowClose {
					// A second collector must not close resources already being released.
					_, err := c.GetOperation("owner", "pty", key)
					if err == nil {
						t.Error("expired session remained recoverable")
					}
				}
				op, err := c.GetOperation("owner", "command", freshKey)
				if err != nil || op.ID != fresh.operation.ID {
					t.Errorf("unrelated lookup: %v %v", op, err)
				}
				retry, owner, err := c.claimOperation("owner", "command", freshKey, "same")
				if err != nil || owner || retry != fresh {
					t.Errorf("duplicate recovery: %v %v", owner, err)
				}
				_, _, err = c.claimOperation("owner", "command", testOperationID(c, "new-command"), "new")
				if err != nil {
					t.Errorf("new admission below capacity: %v", err)
				}
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("unrelated operation blocked behind an expired PTY")
			}
		})
	}
}

func TestOperationExpiryAndPTYLaunchRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		c := NewController("", "")
		c.initOperations()
		now := time.Unix(1700000000, 0)
		c.operations.now = func() time.Time { return now }
		key := testOperationID(c, fmt.Sprintf("launch-race-%08d", i))
		_, err := c.CreatePTYOperation("owner", key, "", "read value")
		require.NoError(t, err)
		op := awaitOperationCreated(t, c, "pty", key)
		session := c.getPTYSession(op.ID)
		require.True(t, session.LockWS())
		now = now.Add(operationRetention + time.Second)
		gate := make(chan struct{})
		launched := make(chan error, 1)
		expired := make(chan bool, 1)
		c.operations.Lock()
		registryKey := operationKey{sha256.Sum256([]byte("owner")), "pty", key}
		entry := c.operations.records[registryKey]
		c.operations.Unlock()
		go func() { <-gate; launched <- session.StartPipe() }()
		go func() { <-gate; expired <- c.expireOperation(registryKey, entry) }()
		close(gate)
		launchErr, wasExpired := <-launched, <-expired
		if launchErr == nil {
			require.False(t, wasExpired)
			recovered, err := c.GetOperation("owner", "pty", key)
			require.NoError(t, err)
			require.Equal(t, op.ID, recovered.ID)
			_, err = session.WriteStdin([]byte("done\n"))
			require.NoError(t, err)
			select {
			case <-session.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("PTY did not exit")
			}
		} else {
			require.True(t, wasExpired)
		}
		require.Error(t, session.StartPipe())
		session.UnlockWS()
		_ = c.DeletePTYSession(op.ID)
	}
}

func TestOperationPTYFailedLaunchNeverReattempts(t *testing.T) {
	for _, pipe := range []bool{false, true} {
		name := "pty"
		if pipe {
			name = "pipe"
		}
		t.Run(name, func(t *testing.T) {
			c := NewController("", "")
			cwd := t.TempDir()
			key := testOperationID(c, "failed-pty-start")
			op, err := c.CreatePTYOperation("owner", key, cwd, "true")
			require.NoError(t, err)
			awaitOperationCreated(t, c, "pty", key)
			s := c.GetPTYSession(op.ID)
			defer c.DeletePTYSession(op.ID)
			require.True(t, s.LockWS())
			defer s.UnlockWS()
			require.NoError(t, os.Remove(cwd))
			start := s.StartPTY
			if pipe {
				start = s.StartPipe
			}
			require.Error(t, start())
			require.NoError(t, os.Mkdir(cwd, 0700))
			require.ErrorContains(t, start(), "already attempted")
			require.False(t, s.IsRunning())
		})
	}
}

func TestOperationPTYRetention(t *testing.T) {
	c := NewController("", "")
	c.initOperations()
	now := time.Unix(1700000000, 0)
	c.operations.now = func() time.Time { return now }
	dormantKey := testOperationID(c, "dormant-pty")
	dormant, err := c.CreatePTYOperation("owner", dormantKey, "", "")
	require.NoError(t, err)
	awaitOperationCreated(t, c, "pty", dormantKey)
	activeKey := testOperationID(c, "active-pty")
	active, err := c.CreatePTYOperation("owner", activeKey, "", "read value")
	require.NoError(t, err)
	awaitOperationCreated(t, c, "pty", activeKey)
	session := c.GetPTYSession(active.ID)
	require.NotNil(t, session)
	require.True(t, session.LockWS())
	defer session.UnlockWS()
	require.NoError(t, session.StartPipe())
	defer c.DeletePTYSession(active.ID)
	now = now.Add(operationRetention + time.Second)
	_, err = c.GetOperation("owner", "pty", dormantKey)
	requireOperationError(t, err, "operation_expired")
	require.Nil(t, c.GetPTYSession(dormant.ID))
	op, err := c.GetOperation("owner", "pty", activeKey)
	require.NoError(t, err)
	require.Equal(t, active.ID, op.ID)
	require.True(t, session.IsRunning())
	_, err = session.WriteStdin([]byte("done\n"))
	require.NoError(t, err)
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("PTY process did not exit")
	}
	// Neither the other launch mode nor the same mode can create a new process.
	require.Error(t, session.StartPipe())
	require.Error(t, session.StartPTY())
	_, err = c.GetOperation("owner", "pty", activeKey)
	requireOperationError(t, err, "operation_expired")
	require.Nil(t, c.GetPTYSession(active.ID))
}

func TestOperationPTYStatusPreservesLegacyRestartOutcome(t *testing.T) {
	c := NewController("", "")
	cwd := t.TempDir()
	id := NewPTYSessionID()
	session, err := c.CreatePTYSession(id, cwd, "exit 7")
	require.NoError(t, err)
	defer c.DeletePTYSession(id)
	require.True(t, session.LockWS())
	defer session.UnlockWS()
	require.NoError(t, session.StartPipe())
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit")
	}
	state, err := c.GetPTYSessionState(id)
	require.NoError(t, err)
	require.True(t, state.LaunchAttempted)
	require.False(t, state.LaunchFailed)
	require.NoError(t, os.Remove(cwd))
	require.Error(t, session.StartPTY())
	state, err = c.GetPTYSessionState(id)
	require.NoError(t, err)
	require.True(t, state.LaunchFailed, "a prior successful launch must not mask a failed legacy restart")
	require.NoError(t, os.Mkdir(cwd, 0700))
	require.NoError(t, session.StartPipe())
	state, err = c.GetPTYSessionState(id)
	require.NoError(t, err)
	require.False(t, state.LaunchFailed)
}
