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
