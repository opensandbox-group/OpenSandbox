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

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

func configureSseLegacyCompat(t *testing.T, enabled bool) {
	t.Helper()
	previous := flag.SseLegacyCompat
	flag.SseLegacyCompat = enabled
	t.Cleanup(func() {
		flag.SseLegacyCompat = previous
	})
}

func TestSseLegacyCompatDisabledKeepsExitCode(t *testing.T) {
	configureSseLegacyCompat(t, false)

	exitCode := 0
	complete := sseLegacyCompatEvent(model.ServerStreamEvent{
		Type:          model.StreamEventTypeComplete,
		ExecutionTime: time.Second.Milliseconds(),
		ExitCode:      &exitCode,
		Timestamp:     time.Now().UnixMilli(),
	})

	require.Contains(t, string(complete.ToJSON()), `"exit_code":0`)
}

func TestSseLegacyCompatDropsExitCodeFromCompleteEvent(t *testing.T) {
	configureSseLegacyCompat(t, true)

	exitCode := 0
	complete := sseLegacyCompatEvent(model.ServerStreamEvent{
		Type:          model.StreamEventTypeComplete,
		ExecutionTime: time.Second.Milliseconds(),
		ExitCode:      &exitCode,
		Timestamp:     time.Now().UnixMilli(),
	})

	require.NotContains(t, string(complete.ToJSON()), "exit_code")
	require.NotZero(t, complete.ExecutionTime)
	require.NotZero(t, complete.Timestamp)
}

func TestSseLegacyCompatDropsExitCodeFromErrorEvent(t *testing.T) {
	configureSseLegacyCompat(t, true)

	exitCode := 3
	original := &execute.ErrorOutput{
		EName:     "CommandExecError",
		EValue:    "3",
		Traceback: []string{"exit status 3"},
		ExitCode:  &exitCode,
	}
	errored := sseLegacyCompatEvent(model.ServerStreamEvent{
		Type:      model.StreamEventTypeError,
		Error:     original,
		Timestamp: time.Now().UnixMilli(),
	})

	payload := string(errored.ToJSON())
	require.Contains(t, payload, `"ename":"CommandExecError"`)
	require.Contains(t, payload, `"evalue":"3"`)
	require.NotContains(t, payload, "exit_code")

	require.NotNil(t, original.ExitCode, "input ErrorOutput must not be mutated")
}

func TestSseLegacyCompatKeepsErrorExitCodeWhenDisabled(t *testing.T) {
	configureSseLegacyCompat(t, false)

	exitCode := 3
	errored := sseLegacyCompatEvent(model.ServerStreamEvent{
		Type: model.StreamEventTypeError,
		Error: &execute.ErrorOutput{
			EName:     "CommandExecError",
			EValue:    "3",
			Traceback: []string{"exit status 3"},
			ExitCode:  &exitCode,
		},
		Timestamp: time.Now().UnixMilli(),
	})

	require.Contains(t, string(errored.ToJSON()), `"exit_code":3`)
}

func TestExecutionTrackerSignalsAfterTheTerminalEvent(t *testing.T) {
	for _, terminal := range []string{"complete", "error"} {
		t.Run(terminal, func(t *testing.T) {
			tracker := newExecutionTracker(context.Background(), "run_code")
			var written []string
			write := func(event string) {
				if len(written) == 0 {
					select {
					case <-tracker.done:
						t.Fatalf("done closed before the %s event was written", event)
					default:
					}
				}
				written = append(written, event)
			}
			hooks := runtime.ExecuteResultHook{
				OnExecuteComplete: func(time.Duration) { write("complete") },
				OnExecuteError:    func(*execute.ErrorOutput) { write("error") },
			}
			tracker.wrap(&hooks)

			if terminal == "complete" {
				hooks.OnExecuteComplete(time.Millisecond)
			} else {
				hooks.OnExecuteError(&execute.ErrorOutput{EName: "Error"})
			}
			// A second terminal callback must not close done twice.
			hooks.OnExecuteError(&execute.ErrorOutput{EName: "Error"})

			select {
			case <-tracker.done:
			default:
				t.Fatal("done not closed after the terminal event")
			}
			require.Equal(t, terminal, written[0])
		})
	}
}
