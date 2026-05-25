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

package runtime

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExecuteCodeRequest_SetDefaultHooks(t *testing.T) {
	customResult := func(map[string]any, int) {}

	req := &ExecuteCodeRequest{
		Hooks: ExecuteResultHook{
			OnExecuteResult: customResult,
		},
	}

	req.SetDefaultHooks()

	require.NotNil(t, req.Hooks.OnExecuteStdout)
	require.NotNil(t, req.Hooks.OnExecuteStderr)
	require.NotNil(t, req.Hooks.OnExecuteError)
	require.NotNil(t, req.Hooks.OnExecuteResult, "expected OnExecuteResult to remain set")
	require.Equal(t, reflect.ValueOf(customResult).Pointer(), reflect.ValueOf(req.Hooks.OnExecuteResult).Pointer(),
		"default hooks should not override existing ones")
}
