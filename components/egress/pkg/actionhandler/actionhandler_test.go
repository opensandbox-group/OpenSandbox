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

package actionhandler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validEnvelopeJSON() string {
	return `{
		"apiVersion": "sandbox.fast.io/actions/v1",
		"operation": "SET_BINDING",
		"invocationId": "sha256:abc",
		"sandbox": {"uid": "u-1", "name": "sandbox-a", "namespace": "tenant-a"},
		"revision": {"specGeneration": 4, "runtimeInstanceId": "rt-1", "attachmentId": "sha256:att", "routeGeneration": 2},
		"attachment": {"network": {"ip": "10.42.0.8", "gateway": "10.42.0.1", "privateCidr": "10.42.0.0/24", "hostVeth": "veth0"}},
		"binding": {"input": "{\"defaultAction\":\"deny\"}"}
	}`
}

func TestParseEnvelopeSetBinding(t *testing.T) {
	env, err := ParseEnvelope([]byte(validEnvelopeJSON()))
	require.NoError(t, err)
	assert.Equal(t, OperationSetBinding, env.Operation)
	assert.Equal(t, "u-1", env.Sandbox.UID)
	assert.Equal(t, "rt-1", env.Revision.RuntimeInstanceID)
	assert.Equal(t, uint64(4), env.Revision.SpecGeneration)
	att := env.Network()
	assert.Equal(t, "10.42.0.8", att.IP.String())
	assert.Equal(t, "10.42.0.1", att.Gateway.String())
	assert.Equal(t, "10.42.0.0/24", att.PrivateCIDR.String())
	assert.Equal(t, "veth0", att.HostVeth)

	val, err := env.Binding.InputString()
	require.NoError(t, err)
	assert.Equal(t, `{"defaultAction":"deny"}`, val)
	assert.False(t, env.Binding.IsRemoval())
}

func TestParseEnvelopeRemovalNullInput(t *testing.T) {
	raw := strings.Replace(validEnvelopeJSON(), `{"input": "{\"defaultAction\":\"deny\"}"}`, `{"input": null}`, 1)
	env, err := ParseEnvelope([]byte(raw))
	require.NoError(t, err)
	require.NotNil(t, env.Binding)
	assert.True(t, env.Binding.IsRemoval())
	// attachment is not required for a removal
	env.Attachment = nil
	require.NoError(t, env.Validate())
}

func TestBindingInputOrdinaryValues(t *testing.T) {
	// "" and the literal string "null" are ordinary handler-owned values
	for _, v := range []string{`""`, `"null"`, `"yaml:\n- a"`} {
		b := Binding{Input: json.RawMessage(v)}
		val, err := b.InputString()
		require.NoError(t, err)
		assert.False(t, b.IsRemoval())
		_ = val
	}
}

func TestParseEnvelopeLifecycleHook(t *testing.T) {
	for _, name := range []string{HookRuntimeReady, HookDataPlaneReady} {
		raw := strings.Replace(validEnvelopeJSON(), `"operation": "SET_BINDING"`, `"operation": "LIFECYCLE_HOOK"`, 1)
		raw = strings.Replace(raw, `"binding": {"input": "{\"defaultAction\":\"deny\"}"}`, `"hook": {"name": "`+name+`", "sequence": 2}`, 1)
		env, err := ParseEnvelope([]byte(raw))
		require.NoError(t, err)
		assert.Equal(t, OperationLifecycleHook, env.Operation)
		require.NotNil(t, env.Hook)
		assert.Equal(t, name, env.Hook.Name)
		assert.Equal(t, uint64(2), env.Hook.Sequence)
	}
}

func TestParseEnvelopeRemoveBinding(t *testing.T) {
	env, err := ParseEnvelope([]byte(`{
		"apiVersion": "sandbox.fast.io/actions/v1",
		"operation": "REMOVE_BINDING",
		"invocationId": "sha256:abc",
		"sandbox": {"uid": "u-1"},
		"revision": {"specGeneration": 4, "runtimeInstanceId": "rt-1", "attachmentId": "sha256:att", "routeGeneration": 2}
	}`))
	require.NoError(t, err)
	assert.Equal(t, OperationRemoveBinding, env.Operation)
	assert.Nil(t, env.Binding)
	assert.Nil(t, env.Hook)
}

func TestParseEnvelopeRejects(t *testing.T) {
	cases := map[string]string{
		"bad apiVersion":     strings.Replace(validEnvelopeJSON(), "sandbox.fast.io/actions/v1", "sandbox.fast.io/actions/v2", 1),
		"unknown operation":  strings.Replace(validEnvelopeJSON(), "SET_BINDING", "EXPLODE", 1),
		"missing uid":        strings.Replace(validEnvelopeJSON(), `"uid": "u-1", `, "", 1),
		"missing binding":    `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"},"attachment":{"network":{"ip":"10.42.0.8","gateway":"10.42.0.1","hostVeth":"veth0"}}}`,
		"non-string input":   strings.Replace(validEnvelopeJSON(), `"{\"defaultAction\":\"deny\"}"`, "42", 1),
		"missing attachment": `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"},"binding":{"input":"{}"}}`,
		"empty veth":         strings.Replace(validEnvelopeJSON(), `"hostVeth": "veth0"`, `"hostVeth": ""`, 1),
		"missing fence":      strings.Replace(validEnvelopeJSON(), `, "runtimeInstanceId": "rt-1"`, "", 1),
		"unknown hook":       `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"LIFECYCLE_HOOK","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"},"hook":{"name":"sandbox.before-delete"}}`,
		"missing hook":       `{"apiVersion":"sandbox.fast.io/actions/v1","operation":"LIFECYCLE_HOOK","sandbox":{"uid":"u"},"revision":{"runtimeInstanceId":"r","attachmentId":"a"}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseEnvelope([]byte(raw))
			require.Error(t, err)
		})
	}
}

func TestStatusResponseJSON(t *testing.T) {
	raw, err := json.Marshal(StatusResponse{APIVersion: APIVersion, Ready: true, InstanceID: "egress-1-2a3b"})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"apiVersion":"sandbox.fast.io/actions/v1"`)
	assert.Contains(t, string(raw), `"ready":true`)
	assert.Contains(t, string(raw), `"instanceId":"egress-1-2a3b"`)
}
