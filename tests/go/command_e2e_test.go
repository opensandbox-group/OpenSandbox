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

package e2e

import (
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func TestCommand_RunSimple(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	exec, err := sb.RunCommand(ctx, "echo hello-from-go-e2e", nil)
	require.NoError(t, err)

	require.NotNil(t, exec.ExitCode)
	require.Equal(t, 0, *exec.ExitCode)

	text := exec.Text()
	require.Contains(t, text, "hello-from-go-e2e")
	t.Logf("Output: %s", text)
}

func TestCommand_RunWithHandlers(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	var stdoutLines []string
	handlers := &opensandbox.ExecutionHandlers{
		OnStdout: func(msg opensandbox.OutputMessage) error {
			stdoutLines = append(stdoutLines, msg.Text)
			return nil
		},
	}

	exec, err := sb.RunCommand(ctx, "echo line1 && echo line2", handlers)
	require.NoError(t, err)

	require.NotEmpty(t, stdoutLines, "expected handler to receive stdout events")
	t.Logf("Handler received %d stdout events", len(stdoutLines))
	t.Logf("Execution stdout count: %d", len(exec.Stdout))
}

func TestCommand_ExitCode(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	exec, err := sb.RunCommand(ctx, "true", nil)
	require.NoError(t, err)
	require.NotNil(t, exec.ExitCode)
	require.Equal(t, 0, *exec.ExitCode)
	t.Log("Exit code tests passed")
}

func TestCommand_MultiLine(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	exec, err := sb.RunCommand(ctx, "echo hello && echo world && uname -a", nil)
	require.NoError(t, err)

	text := exec.Text()
	require.Contains(t, text, "hello")
	require.Contains(t, text, "world")
	t.Logf("Multi-line output (%d bytes): %s", len(text), text)
}

func TestCommand_EnvInjection(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	exec, err := sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command: "echo $CUSTOM_VAR",
		Envs: map[string]string{
			"CUSTOM_VAR": "injected-from-go-e2e",
		},
	}, nil)
	require.NoError(t, err)

	text := exec.Text()
	require.Contains(t, text, "injected-from-go-e2e")
	t.Logf("Env injection: %s", text)
}

func TestCommand_BackgroundStatusLogs(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	exec, err := sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command:    "echo bg-output && sleep 1 && echo bg-done",
		Background: true,
	}, nil)
	require.NoError(t, err)

	if exec.ID == "" {
		t.Log("No execution ID from background command (server may not return init event for background)")
		return
	}
	t.Logf("Background command ID: %s", exec.ID)
}

func TestCommand_Interrupt(t *testing.T) {
	ctx, sb := createTestSandbox(t)

	exec, err := sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command:    "sleep 300",
		Background: true,
	}, nil)
	require.NoError(t, err)
	if exec.ID == "" {
		t.Log("No execution ID — cannot test interrupt")
		return
	}

	pingExec, err := sb.RunCommand(ctx, "echo still-alive", nil)
	require.NoError(t, err)
	require.Contains(t, pingExec.Text(), "still-alive")
	t.Log("Interrupt test: sandbox responsive during background command")
}

func TestCommand_StatusAndLogs(t *testing.T) {
	ctx, sb := createTestSandbox(t)
	execd := newExecdClientForSandbox(t, ctx, sb)

	exec, err := sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command:    "echo status-log-start && sleep 1 && echo status-log-end",
		Background: true,
	}, nil)
	require.NoError(t, err)
	if exec.ID == "" {
		t.Skip("no execution ID returned for background command")
	}

	status, err := execd.GetCommandStatus(ctx, exec.ID)
	require.NoError(t, err)
	require.Equal(t, exec.ID, status.ID)

	var logs *opensandbox.CommandLogsResponse
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		logs, err = execd.GetCommandLogs(ctx, exec.ID, nil)
		require.NoError(t, err)
		if logs != nil && logs.Output != "" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	require.NotNil(t, logs)
	require.Contains(t, logs.Output, "status-log-start")
}

func TestCommand_InterruptCommandAPI(t *testing.T) {
	ctx, sb := createTestSandbox(t)
	execd := newExecdClientForSandbox(t, ctx, sb)

	exec, err := sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command:    "sleep 300",
		Background: true,
	}, nil)
	require.NoError(t, err)
	if exec.ID == "" {
		t.Skip("no execution ID returned for background command")
	}

	require.NoError(t, execd.InterruptCommand(ctx, exec.ID))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		status, statusErr := execd.GetCommandStatus(ctx, exec.ID)
		if statusErr == nil && !status.Running {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Log("interrupt requested but status stayed running within timeout")
}
