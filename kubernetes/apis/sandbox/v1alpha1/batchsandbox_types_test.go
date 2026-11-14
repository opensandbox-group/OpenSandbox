// Copyright 2025 The OpenSandbox Authors
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

package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchSandboxLegacyShardTaskPatchDecodesLeniently(t *testing.T) {
	const manifest = `{
		"apiVersion":"sandbox.opensandbox.io/v1alpha1",
		"kind":"BatchSandbox",
		"metadata":{"name":"legacy","namespace":"default"},
		"spec":{"shardTaskPatches":[{
			"spec":{
				"process":{"command":["sleep"],"args":[3600]},
				"timeoutSeconds":"legacy-invalid"
			}
		}]}
	}`

	var sandbox BatchSandbox
	require.NoError(t, json.Unmarshal([]byte(manifest), &sandbox))

	patch := sandbox.Spec.ShardTaskPatches[0].Spec
	require.NotNil(t, patch.Process)
	require.NotNil(t, patch.Process.Command)
	require.Equal(t, []string{"sleep"}, *patch.Process.Command)
	require.Nil(t, patch.Process.Args)
	require.JSONEq(t, `[3600]`, string(patch.Process.Extra["args"]))
	require.NotContains(t, patch.Process.Extra, "command")
	require.Nil(t, patch.TimeoutSeconds)
	require.JSONEq(t, `"legacy-invalid"`, string(patch.Extra["timeoutSeconds"]))

	encoded, err := json.Marshal(patch)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"process":{"command":["sleep"],"args":[3600]},
		"timeoutSeconds":"legacy-invalid"
	}`, string(encoded))
}

func TestTaskSpecPatchTimeoutOnlyOmitsProcess(t *testing.T) {
	timeoutSeconds := int64(600)
	patch := TaskSpecPatch{TimeoutSeconds: &timeoutSeconds}

	encoded, err := json.Marshal(patch)
	require.NoError(t, err)
	require.JSONEq(t, `{"timeoutSeconds":600}`, string(encoded))
}

func TestProcessTaskPatchDoesNotResurrectConsumedFields(t *testing.T) {
	var patch ProcessTaskPatch
	require.NoError(t, json.Unmarshal([]byte(`{
		"command":["echo"],
		"args":["hello"],
		"workingDir":"/tmp"
	}`), &patch))

	require.NotContains(t, patch.Extra, "command")
	require.NotContains(t, patch.Extra, "args")
	patch.Command = nil

	encoded, err := json.Marshal(patch)
	require.NoError(t, err)
	require.JSONEq(t, `{"args":["hello"],"workingDir":"/tmp"}`, string(encoded))
}
