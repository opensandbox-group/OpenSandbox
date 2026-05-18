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
// +build !windows

package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/internal/safego"
)

func TestBashSession_NonZeroExitEmitsError(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	c := NewController("", "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		sessionID  string
		stdoutLine string
		errCh      = make(chan *execute.ErrorOutput, 1)
		completeCh = make(chan struct{}, 1)
	)

	req := &ExecuteCodeRequest{
		Language: Bash,
		Code:     `echo "before"; exit 7`,
		Cwd:      t.TempDir(),
		Timeout:  5 * time.Second,
		Hooks: ExecuteResultHook{
			OnExecuteInit:   func(s string) { sessionID = s },
			OnExecuteStdout: func(_ int64, s string) { stdoutLine = s },
			OnExecuteError:  func(err *execute.ErrorOutput) { errCh <- err },
			OnExecuteComplete: func(_ time.Duration) {
				completeCh <- struct{}{}
			},
		},
	}

	session, err := c.createBashSession(&CreateContextRequest{})
	assert.NoError(t, err)
	req.Context = session
	require.NoError(t, c.runBashSession(ctx, req))

	var gotErr *execute.ErrorOutput
	select {
	case gotErr = <-errCh:
	case <-time.After(2 * time.Second):
		require.Fail(t, "expected error hook to be called")
	}
	require.NotNil(t, gotErr, "expected non-nil error output")
	require.Equal(t, "CommandExecError", gotErr.EName)
	require.Equal(t, "7", gotErr.EValue)
	require.NotEmpty(t, sessionID, "expected session id to be set")
	require.Equal(t, "before", stdoutLine)

	select {
	case <-completeCh:
		require.Fail(t, "did not expect completion hook on non-zero exit")
	default:
	}
}

func TestBashSession_envAndExitCode(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	var (
		initCalls     int
		completeCalls int
		stdoutLines   []string
	)

	hooks := ExecuteResultHook{
		OnExecuteInit: func(ctx string) {
			require.Equal(t, session.config.Session, ctx, "unexpected session in OnExecuteInit")
			initCalls++
		},
		OnExecuteStdout: func(_ int64, text string) {
			t.Log(text)
			stdoutLines = append(stdoutLines, text)
		},
		OnExecuteComplete: func(_ time.Duration) {
			completeCalls++
		},
	}

	// 1) export an env var
	request := &ExecuteCodeRequest{
		Code:    "export FOO=hello",
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	}
	require.NoError(t, session.run(context.Background(), request))
	exportStdoutCount := len(stdoutLines)

	// 2) verify env is persisted
	request = &ExecuteCodeRequest{
		Code:    "echo $FOO",
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	}
	require.NoError(t, session.run(context.Background(), request))
	echoLines := stdoutLines[exportStdoutCount:]
	foundHello := false
	for _, line := range echoLines {
		if strings.TrimSpace(line) == "hello" {
			foundHello = true
			break
		}
	}
	require.True(t, foundHello, "expected echo $FOO to output 'hello', got %v", echoLines)

	// 3) ensure exit code of previous command is reflected in shell state
	request = &ExecuteCodeRequest{
		Code:    "false; echo EXIT:$?",
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	}
	prevCount := len(stdoutLines)
	require.NoError(t, session.run(context.Background(), request))
	exitLines := stdoutLines[prevCount:]
	foundExit := false
	for _, line := range exitLines {
		if strings.Contains(line, "EXIT:1") {
			foundExit = true
			break
		}
	}
	require.True(t, foundExit, "expected exit code output 'EXIT:1', got %v", exitLines)
	require.Equal(t, 3, initCalls, "OnExecuteInit expected 3 calls")
	require.Equal(t, 3, completeCalls, "OnExecuteComplete expected 3 calls")
}

func TestBashSession_envLargeOutputChained(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	var (
		initCalls     int
		completeCalls int
		stdoutLines   []string
	)

	hooks := ExecuteResultHook{
		OnExecuteInit: func(ctx string) {
			require.Equal(t, session.config.Session, ctx, "unexpected session in OnExecuteInit")
			initCalls++
		},
		OnExecuteStdout: func(_ int64, text string) {
			t.Log(text)
			stdoutLines = append(stdoutLines, text)
		},
		OnExecuteComplete: func(_ time.Duration) {
			completeCalls++
		},
	}

	runAndCollect := func(cmd string) []string {
		start := len(stdoutLines)
		request := &ExecuteCodeRequest{
			Code:    cmd,
			Hooks:   hooks,
			Timeout: 10 * time.Second,
		}
		require.NoError(t, session.run(context.Background(), request))
		return append([]string(nil), stdoutLines[start:]...)
	}

	lines1 := runAndCollect("export FOO=hello1; for i in $(seq 1 60); do echo A${i}:$FOO; done")
	require.GreaterOrEqual(t, len(lines1), 60, "expected >=60 lines for cmd1")
	require.True(t, containsLine(lines1, "A1:hello1") && containsLine(lines1, "A60:hello1"), "env not reflected in cmd1 output, got %v", lines1[:3])

	lines2 := runAndCollect("export FOO=${FOO}_next; export BAR=bar1; for i in $(seq 1 60); do echo B${i}:$FOO:$BAR; done")
	require.GreaterOrEqual(t, len(lines2), 60, "expected >=60 lines for cmd2")
	require.True(t, containsLine(lines2, "B1:hello1_next:bar1") && containsLine(lines2, "B60:hello1_next:bar1"), "env not propagated to cmd2 output, sample %v", lines2[:3])

	lines3 := runAndCollect("export BAR=${BAR}_last; for i in $(seq 1 60); do echo C${i}:$FOO:$BAR; done; echo FINAL_FOO=$FOO; echo FINAL_BAR=$BAR")
	require.GreaterOrEqual(t, len(lines3), 62, "expected >=62 lines for cmd3") // 60 lines + 2 finals
	require.True(t, containsLine(lines3, "C1:hello1_next:bar1_last") && containsLine(lines3, "C60:hello1_next:bar1_last"), "env not propagated to cmd3 output, sample %v", lines3[:3])
	require.True(t, containsLine(lines3, "FINAL_FOO=hello1_next") && containsLine(lines3, "FINAL_BAR=bar1_last"), "final env lines missing, got %v", lines3[len(lines3)-5:])
	require.Equal(t, 3, initCalls, "OnExecuteInit expected 3 calls")
	require.Equal(t, 3, completeCalls, "OnExecuteComplete expected 3 calls")
}

func TestBashSession_cwdPersistsWithoutOverride(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	targetDir := t.TempDir()
	var stdoutLines []string
	hooks := ExecuteResultHook{
		OnExecuteStdout: func(_ int64, line string) {
			stdoutLines = append(stdoutLines, line)
		},
	}

	runAndCollect := func(req *ExecuteCodeRequest) []string {
		start := len(stdoutLines)
		require.NoError(t, session.run(context.Background(), req))
		return append([]string(nil), stdoutLines[start:]...)
	}

	firstRunLines := runAndCollect(&ExecuteCodeRequest{
		Code:    fmt.Sprintf("cd %s\npwd", targetDir),
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	})
	require.True(t, containsLine(firstRunLines, targetDir), "expected cd to update cwd to %q, got %v", targetDir, firstRunLines)

	secondRunLines := runAndCollect(&ExecuteCodeRequest{
		Code:    "pwd",
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	})
	require.True(t, containsLine(secondRunLines, targetDir), "expected subsequent run to inherit cwd %q, got %v", targetDir, secondRunLines)

	session.mu.Lock()
	finalCwd := session.cwd
	session.mu.Unlock()
	require.Equal(t, targetDir, finalCwd, "expected session cwd to stay at %q", targetDir)
}

func TestBashSession_requestCwdOverridesAfterCd(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	initialDir := t.TempDir()
	overrideDir := t.TempDir()

	var stdoutLines []string
	hooks := ExecuteResultHook{
		OnExecuteStdout: func(_ int64, line string) {
			stdoutLines = append(stdoutLines, line)
		},
	}

	runAndCollect := func(req *ExecuteCodeRequest) []string {
		start := len(stdoutLines)
		require.NoError(t, session.run(context.Background(), req))
		return append([]string(nil), stdoutLines[start:]...)
	}

	// First request: change session cwd via script.
	firstRunLines := runAndCollect(&ExecuteCodeRequest{
		Code:    fmt.Sprintf("cd %s\npwd", initialDir),
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	})
	require.True(t, containsLine(firstRunLines, initialDir), "expected cd to update cwd to %q, got %v", initialDir, firstRunLines)

	// Second request: explicit Cwd overrides session cwd.
	secondRunLines := runAndCollect(&ExecuteCodeRequest{
		Code:    "pwd",
		Cwd:     overrideDir,
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	})
	require.True(t, containsLine(secondRunLines, overrideDir), "expected command to run in override cwd %q, got %v", overrideDir, secondRunLines)

	session.mu.Lock()
	finalCwd := session.cwd
	session.mu.Unlock()
	require.Equal(t, overrideDir, finalCwd, "expected session cwd updated to override dir %q", overrideDir)
}

func TestBashSession_envDumpNotLeakedWhenNoTrailingNewline(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	var stdoutLines []string
	hooks := ExecuteResultHook{
		OnExecuteStdout: func(_ int64, line string) {
			stdoutLines = append(stdoutLines, line)
		},
	}

	request := &ExecuteCodeRequest{
		Code:    `set +x; printf '{"foo":1}'`,
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	}
	require.NoError(t, session.run(context.Background(), request))

	require.Len(t, stdoutLines, 1, "expected exactly one stdout line")
	require.Equal(t, `{"foo":1}`, strings.TrimSpace(stdoutLines[0]))
	for _, line := range stdoutLines {
		require.NotContains(t, line, envDumpStartMarker, "env dump leaked into stdout: %v", stdoutLines)
		require.NotContains(t, line, "declare -x", "env dump leaked into stdout: %v", stdoutLines)
	}
}

func TestBashSession_envDumpNotLeakedWhenNoOutput(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	var stdoutLines []string
	hooks := ExecuteResultHook{
		OnExecuteStdout: func(_ int64, line string) {
			stdoutLines = append(stdoutLines, line)
		},
	}

	request := &ExecuteCodeRequest{
		Code:    `set +x; true`,
		Hooks:   hooks,
		Timeout: 3 * time.Second,
	}
	require.NoError(t, session.run(context.Background(), request))

	require.LessOrEqual(t, len(stdoutLines), 1, "expected at most one stdout line, got %v", stdoutLines)
	if len(stdoutLines) == 1 {
		require.Empty(t, strings.TrimSpace(stdoutLines[0]), "expected empty stdout")
	}
	for _, line := range stdoutLines {
		require.NotContains(t, line, envDumpStartMarker, "env dump leaked into stdout: %v", stdoutLines)
		require.NotContains(t, line, "declare -x", "env dump leaked into stdout: %v", stdoutLines)
	}
}

func TestBashSession_heredoc(t *testing.T) {
	rewardDir := t.TempDir()
	controller := NewController("", "")

	sessionID, err := controller.CreateBashSession(&CreateContextRequest{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = controller.DeleteBashSession(sessionID) })

	hooks := ExecuteResultHook{
		OnExecuteStdout: func(_ int64, line string) {
			fmt.Printf("[stdout] %s\n", line)
		},
		OnExecuteComplete: func(d time.Duration) {
			fmt.Printf("[complete] %s\n", d)
		},
	}

	// First run: heredoc + reward file write.
	script := fmt.Sprintf(`
set -x
reward_dir=%q
mkdir -p "$reward_dir"

cat > /tmp/repro_script.sh <<'SHEOF'
#!/usr/bin/env sh
echo "hello heredoc"
SHEOF

chmod +x /tmp/repro_script.sh
/tmp/repro_script.sh
echo "after heredoc"
echo 1 > "$reward_dir/reward.txt"
cat "$reward_dir/reward.txt"
`, rewardDir)

	ctx := context.Background()
	require.NoError(t, controller.RunInBashSession(ctx, &ExecuteCodeRequest{
		Context:  sessionID,
		Language: Bash,
		Timeout:  10 * time.Second,
		Code:     script,
		Hooks:    hooks,
	}))

	// Second run: ensure the session keeps working.
	require.NoError(t, controller.RunInBashSession(ctx, &ExecuteCodeRequest{
		Context:  sessionID,
		Language: Bash,
		Timeout:  5 * time.Second,
		Code:     "echo 'second command works'",
		Hooks:    hooks,
	}))
}

func TestBashSession_execReplacesShell(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	var stdoutLines []string
	hooks := ExecuteResultHook{
		OnExecuteStdout: func(_ int64, line string) {
			stdoutLines = append(stdoutLines, line)
		},
	}

	script := `
cat > /tmp/exec_child.sh <<'EOF'
echo "child says hi"
EOF
chmod +x /tmp/exec_child.sh
exec /tmp/exec_child.sh
`

	request := &ExecuteCodeRequest{
		Code:    script,
		Hooks:   hooks,
		Timeout: 5 * time.Second,
	}
	require.NoError(t, session.run(context.Background(), request), "expected exec to complete without killing the session")
	require.True(t, containsLine(stdoutLines, "child says hi"), "expected child output, got %v", stdoutLines)

	// Subsequent run should still work because we restart bash per run.
	request = &ExecuteCodeRequest{
		Code:    "echo still-alive",
		Hooks:   hooks,
		Timeout: 2 * time.Second,
	}
	stdoutLines = nil
	require.NoError(t, session.run(context.Background(), request), "expected run to succeed after exec replaced the shell")
	require.True(t, containsLine(stdoutLines, "still-alive"), "expected follow-up output, got %v", stdoutLines)
}

func TestBashSession_complexExec(t *testing.T) {
	session := newBashSession("")
	t.Cleanup(func() { _ = session.close() })

	require.NoError(t, session.start())

	var stdoutLines []string
	hooks := ExecuteResultHook{
		OnExecuteStdout: func(_ int64, line string) {
			stdoutLines = append(stdoutLines, line)
		},
	}

	script := `
LOG_FILE=$(mktemp)
export LOG_FILE
exec 3>&1 4>&2
exec > >(tee "$LOG_FILE") 2>&1

set -x
echo "from-complex-exec"
exec 1>&3 2>&4 # step record
echo "after-restore"
`

	request := &ExecuteCodeRequest{
		Code:    script,
		Hooks:   hooks,
		Timeout: 5 * time.Second,
	}
	require.NoError(t, session.run(context.Background(), request), "expected complex exec to finish")
	require.True(t, containsLine(stdoutLines, "from-complex-exec") && containsLine(stdoutLines, "after-restore"), "expected exec outputs, got %v", stdoutLines)

	// Session should still be usable.
	request = &ExecuteCodeRequest{
		Code:    "echo still-alive",
		Hooks:   hooks,
		Timeout: 2 * time.Second,
	}
	stdoutLines = nil
	require.NoError(t, session.run(context.Background(), request), "expected run to succeed after complex exec")
	require.True(t, containsLine(stdoutLines, "still-alive"), "expected follow-up output, got %v", stdoutLines)
}

func containsLine(lines []string, target string) bool {
	for _, l := range lines {
		if strings.TrimSpace(l) == target {
			return true
		}
	}
	return false
}

// TestBashSession_CloseKillsRunningProcess verifies that session.close() kills the active
// process group so that a long-running command (e.g. sleep) does not keep running after close.
func TestBashSession_CloseKillsRunningProcess(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	session := newBashSession("")
	require.NoError(t, session.start())

	runDone := make(chan error, 1)
	req := &ExecuteCodeRequest{
		Code:    "sleep 30",
		Timeout: 60 * time.Second,
		Hooks:   ExecuteResultHook{},
	}
	safego.Go(func() {
		runDone <- session.run(context.Background(), req)
	})

	// Give the child process time to start.
	time.Sleep(200 * time.Millisecond)

	// Close should kill the process group; run() should return soon (it may return nil
	// because the code path treats non-zero exit as success after calling OnExecuteError).
	require.NoError(t, session.close())

	select {
	case <-runDone:
		// run() returned; process was killed so we did not wait 30s
	case <-time.After(3 * time.Second):
		require.Fail(t, "run did not return within 3s after close (process was not killed)")
	}
}

// TestBashSession_DeleteBashSessionKillsRunningProcess verifies that DeleteBashSession
// (close path) kills the active run and removes the session from the controller.
func TestBashSession_DeleteBashSessionKillsRunningProcess(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH")
	}

	c := NewController("", "")
	sessionID, err := c.CreateBashSession(&CreateContextRequest{})
	require.NoError(t, err)

	runDone := make(chan error, 1)
	req := &ExecuteCodeRequest{
		Language: Bash,
		Context:  sessionID,
		Code:     "sleep 30",
		Timeout:  60 * time.Second,
		Hooks:    ExecuteResultHook{},
	}
	safego.Go(func() {
		runDone <- c.RunInBashSession(context.Background(), req)
	})

	time.Sleep(200 * time.Millisecond)

	require.NoError(t, c.DeleteBashSession(sessionID))

	select {
	case <-runDone:
		// RunInBashSession returned; process was killed
	case <-time.After(3 * time.Second):
		require.Fail(t, "RunInBashSession did not return within 3s after DeleteBashSession")
	}

	// Session should be gone; deleting again should return ErrContextNotFound.
	err = c.DeleteBashSession(sessionID)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextNotFound)
}

// TestBashSession_CloseWithNoActiveRun verifies that close() with no running command
// completes without error and does not hang.
func TestBashSession_CloseWithNoActiveRun(t *testing.T) {
	session := newBashSession("")
	require.NoError(t, session.start())

	done := make(chan struct{}, 1)
	safego.Go(func() {
		_ = session.close()
		done <- struct{}{}
	})

	select {
	case <-done:
		// close() returned
	case <-time.After(2 * time.Second):
		require.Fail(t, "close() did not return within 2s when no run was active")
	}
}
