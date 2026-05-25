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
// +build !windows

package runtime

import (
	"bufio"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/internal/safego"
)

// replayContains polls the replay buffer until it contains substr or timeout expires.
func replayContains(t *testing.T, s *ptySession, substr string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := s.replay.ReadFrom(0)
		if strings.Contains(string(data), substr) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func TestPTYSession_BasicExecution(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPTY())
	t.Cleanup(func() { s.close() })

	stdoutR, _, detach := s.AttachOutput()
	defer detach()
	safego.Go(func() { _, _ = io.Copy(io.Discard, stdoutR) }) //nolint:errcheck

	_, err := s.WriteStdin([]byte("echo hello_pty\n"))
	require.NoError(t, err)

	require.True(t, replayContains(t, s, "hello_pty", 5*time.Second),
		"expected 'hello_pty' in PTY replay buffer")
}

func TestPTYSession_IsRunning(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.False(t, s.IsRunning())
	require.NoError(t, s.StartPTY())
	t.Cleanup(func() { s.close() })
	require.True(t, s.IsRunning())
}

func TestPTYSession_ResizeWinsize(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPTY())
	t.Cleanup(func() { s.close() })

	// Attach output so the broadcast goroutine has a sink and fills the replay buffer.
	stdoutR, _, detach := s.AttachOutput()
	defer detach()
	safego.Go(func() { _, _ = io.Copy(io.Discard, stdoutR) }) //nolint:errcheck

	// Wait for bash to start (prompt appears).
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, s.ResizePTY(120, 40))

	// stty size reports "rows cols" (e.g. "40 120").
	_, err := s.WriteStdin([]byte("stty size\n"))
	require.NoError(t, err)

	require.True(t, replayContains(t, s, "40 120", 5*time.Second),
		"expected 'stty size' output '40 120' in replay buffer")
}

func TestPTYSession_ANSISequences(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPTY())
	t.Cleanup(func() { s.close() })

	stdoutR, _, detach := s.AttachOutput()
	defer detach()
	safego.Go(func() { _, _ = io.Copy(io.Discard, stdoutR) }) //nolint:errcheck

	// Send printf with explicit ESC bytes via $'\033'.
	_, err := s.WriteStdin([]byte("printf $'\\033[1;32mGREEN\\033[0m\\n'\n"))
	require.NoError(t, err)

	// Wait for ESC from printf output. Do not match on "GREEN" alone: the echoed
	// command line contains that substring before printf runs.
	require.True(t, replayContains(t, s, "\x1b", 5*time.Second),
		"expected ESC bytes in PTY replay buffer")

	data, _ := s.replay.ReadFrom(0)
	assert.Contains(t, string(data), "GREEN", "expected GREEN text in PTY output")
}

func TestPTYSession_PipeMode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPipe())
	t.Cleanup(func() { s.close() })

	require.False(t, s.IsPTY())

	stdoutR, stderrR, detach := s.AttachOutput()
	defer detach()
	require.NotNil(t, stderrR)

	stdoutCh := make(chan string, 32)
	safego.Go(func() {
		sc := bufio.NewScanner(stdoutR)
		for sc.Scan() {
			stdoutCh <- sc.Text()
		}
	})

	stderrCh := make(chan string, 32)
	safego.Go(func() {
		sc := bufio.NewScanner(stderrR)
		for sc.Scan() {
			stderrCh <- sc.Text()
		}
	})

	_, err := s.WriteStdin([]byte("echo hello_pipe\necho err_pipe >&2\n"))
	require.NoError(t, err)

	require.True(t, waitForLine(stdoutCh, "hello_pipe", 5*time.Second),
		"expected 'hello_pipe' on stdout")
	require.True(t, waitForLine(stderrCh, "err_pipe", 5*time.Second),
		"expected 'err_pipe' on stderr")
}

func TestPTYSession_ReconnectReplay(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPTY())
	t.Cleanup(func() { s.close() })

	// First connection — drain output so replay buffer fills.
	stdoutR1, _, detach1 := s.AttachOutput()
	safego.Go(func() { _, _ = io.Copy(io.Discard, stdoutR1) }) //nolint:errcheck

	_, err := s.WriteStdin([]byte("echo first_output\n"))
	require.NoError(t, err)

	require.True(t, replayContains(t, s, "first_output", 5*time.Second),
		"expected 'first_output' in replay buffer")

	snapshot, snapshotOff := s.replay.ReadFrom(0)
	detach1()
	time.Sleep(50 * time.Millisecond)

	// Reconnect — replay from offset 0 should return the same bytes we snapshotted.
	replay, replayOff := s.replay.ReadFrom(0)
	require.Equal(t, snapshotOff, replayOff)
	// The new snapshot may be larger (more output arrived), but must contain snapshot.
	require.True(t, strings.HasPrefix(string(replay), string(snapshot)) || len(replay) >= len(snapshot),
		"replay should contain at least the original snapshot bytes")

	// Second connection.
	stdoutR2, _, detach2 := s.AttachOutput()
	defer detach2()
	safego.Go(func() { _, _ = io.Copy(io.Discard, stdoutR2) }) //nolint:errcheck

	offsetAfterFirst := int64(len(replay))

	_, err = s.WriteStdin([]byte("echo second_output\n"))
	require.NoError(t, err)

	require.True(t, replayContains(t, s, "second_output", 5*time.Second),
		"expected 'second_output' in replay buffer")

	// Delta replay from offset-after-first should contain only new bytes.
	newData, _ := s.replay.ReadFrom(offsetAfterFirst)
	require.True(t, strings.Contains(string(newData), "second_output"),
		"delta replay should contain 'second_output', got %q", string(newData))
}

func TestPTYSession_SendSIGINT(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPTY())
	t.Cleanup(func() { s.close() })

	stdoutR, _, detach := s.AttachOutput()
	defer detach()
	safego.Go(func() { _, _ = io.Copy(io.Discard, stdoutR) }) //nolint:errcheck

	// Start a sleep inside the PTY.
	_, err := s.WriteStdin([]byte("sleep 30\n"))
	require.NoError(t, err)
	time.Sleep(200 * time.Millisecond)

	// SIGINT interrupts the sleep; bash continues.
	s.SendSignal("SIGINT")

	// Bash itself should still be alive.
	require.Eventually(t, func() bool {
		return s.IsRunning()
	}, 2*time.Second, 50*time.Millisecond, "bash should still be running after SIGINT")
}

func TestPTYSession_CloseTerminatesProcess(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPTY())

	require.True(t, s.IsRunning())
	s.close()

	done := s.Done()
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("process did not exit within 3s after close()")
		}
	}
}

func TestPTYSession_ExitCode(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	s := newPTYSession(uuidString(), "")
	require.NoError(t, s.StartPTY())

	stdoutR, _, detach := s.AttachOutput()
	safego.Go(func() { _, _ = io.Copy(io.Discard, stdoutR) }) //nolint:errcheck

	_, _ = s.WriteStdin([]byte("exit 42\n"))

	done := s.Done()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		detach()
		t.Fatal("bash did not exit within 5s")
	}
	detach()

	require.Equal(t, 42, s.ExitCode())
}

func TestPTYSession_LockWS(t *testing.T) {
	s := newPTYSession(uuidString(), "")
	require.True(t, s.LockWS(), "first lock should succeed")
	require.False(t, s.LockWS(), "second lock should fail")
	s.UnlockWS()
	require.True(t, s.LockWS(), "lock after unlock should succeed")
	s.UnlockWS()
}

func TestPTYSession_ControllerCRUD(t *testing.T) {
	c := NewController("", "")
	id := uuidString()

	sess, _ := c.CreatePTYSession(id, "")
	require.NotNil(t, sess)

	got := c.GetPTYSession(id)
	require.NotNil(t, got)
	require.Equal(t, id, got.(*ptySession).id)

	running, offset, err := c.GetPTYSessionStatus(id)
	require.NoError(t, err)
	require.False(t, running)
	require.Equal(t, int64(0), offset)

	require.NoError(t, c.DeletePTYSession(id))
	require.Nil(t, c.GetPTYSession(id))

	require.ErrorIs(t, c.DeletePTYSession(id), ErrContextNotFound)
}

// waitForLine reads from ch until target is found or timeout expires.
func waitForLine(ch <-chan string, target string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case line := <-ch:
			if strings.Contains(line, target) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// discardAll drains r until EOF.
func discardAll(r io.Reader) {
	_, _ = io.Copy(io.Discard, r)
}
