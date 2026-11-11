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

package runtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
)

func TestDispatchExecutionResultHooks_ErrorSkipsComplete(t *testing.T) {
	var (
		errorCalls    int
		completeCalls int
	)

	req := &ExecuteCodeRequest{
		Hooks: ExecuteResultHook{
			OnExecuteError: func(_ *execute.ErrorOutput) {
				errorCalls++
			},
			OnExecuteComplete: func(_ time.Duration) {
				completeCalls++
			},
		},
	}

	dispatchExecutionResultHooks(req, &execute.ExecutionResult{
		ExecutionTime: 35 * time.Millisecond,
		Error: &execute.ErrorOutput{
			EName:  "RuntimeError",
			EValue: "boom",
		},
	})

	require.Equal(t, 1, errorCalls)
	require.Equal(t, 0, completeCalls)
}

func TestDispatchExecutionResultHooks_SuccessEmitsComplete(t *testing.T) {
	var (
		errorCalls    int
		completeCalls int
	)

	req := &ExecuteCodeRequest{
		Hooks: ExecuteResultHook{
			OnExecuteError: func(_ *execute.ErrorOutput) {
				errorCalls++
			},
			OnExecuteComplete: func(_ time.Duration) {
				completeCalls++
			},
		},
	}

	dispatchExecutionResultHooks(req, &execute.ExecutionResult{
		ExecutionTime: 50 * time.Millisecond,
	})

	require.Equal(t, 0, errorCalls)
	require.Equal(t, 1, completeCalls)
}

func TestDispatchExecutionResultHooks_TerminalEventFollowsOutput(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "success"
		terminalEvent := "complete"
		var errOutput *execute.ErrorOutput
		if failed {
			name = "failure"
			terminalEvent = "error"
			errOutput = &execute.ErrorOutput{EName: "ValueError", EValue: "failed cell"}
		}
		t.Run(name, func(t *testing.T) {
			var events []string
			req := &ExecuteCodeRequest{Hooks: ExecuteResultHook{
				OnExecuteResult: func(_ map[string]interface{}, _ int) {
					events = append(events, "result")
				},
				OnExecuteStdout: func(_ string) { events = append(events, "stdout") },
				OnExecuteStderr: func(_ string) { events = append(events, "stderr") },
				OnExecuteError: func(_ *execute.ErrorOutput) {
					events = append(events, "error")
				},
				OnExecuteComplete: func(_ time.Duration) {
					events = append(events, "complete")
				},
			}}

			dispatchExecutionResultHooks(req, &execute.ExecutionResult{
				ExecutionCount: 1,
				ExecutionData:  map[string]interface{}{"text/plain": "4"},
				Stream: []*execute.StreamOutput{
					{Name: execute.StreamStdout, Text: "output"},
					{Name: execute.StreamStderr, Text: "diagnostic"},
				},
				Error:         errOutput,
				ExecutionTime: 50 * time.Millisecond,
			})

			require.Equal(t, []string{"result", "stdout", "stderr", terminalEvent}, events)
		})
	}
}
