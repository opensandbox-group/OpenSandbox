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

package idlepolicy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAcceptsWakeOnlyAndPausePolicies(t *testing.T) {
	// Wake-only policy: threshold/action absent, wake defaults on.
	policy, err := Parse(`{"wakeOnAccess": false}`)
	require.NoError(t, err)
	require.Nil(t, policy.IdleTimeoutSeconds)
	require.NotNil(t, policy.WakeOnAccess)
	require.False(t, *policy.WakeOnAccess)

	// Full pause policy with a probe.
	policy, err = Parse(`{"idleTimeoutSeconds":300,"onIdle":"pause","wakeOnAccess":true,"activeProbe":{"port":8080,"path":"/active","timeoutSeconds":2}}`)
	require.NoError(t, err)
	require.Equal(t, 300, *policy.IdleTimeoutSeconds)
	require.Equal(t, OnIdlePause, policy.OnIdle)
	require.True(t, policy.WakeOnAccess == nil || *policy.WakeOnAccess)
}

func TestParseFailsSafe(t *testing.T) {
	for name, raw := range map[string]string{
		"malformed json":      `{"idleTimeoutSeconds":`,
		"unknown field":       `{"idleTimeoutSeconds":300,"onIdle":"pause","wakeOnAccess":true,"futureField":1}`,
		"threshold below 30":  `{"idleTimeoutSeconds":29,"onIdle":"pause"}`,
		"threshold above max": `{"idleTimeoutSeconds":86401,"onIdle":"pause"}`,
		"unsupported onIdle":  `{"idleTimeoutSeconds":300,"onIdle":"terminate"}`,
		"onIdle alone":        `{"onIdle":"pause"}`,
		"threshold alone":     `{"idleTimeoutSeconds":300}`,
		"wrong type":          `{"idleTimeoutSeconds":"300","onIdle":"pause"}`,
		"json null":           `null`,
		"empty":               ``,
		"whitespace":          `   `,
		"trailing garbage":    `{"wakeOnAccess":true} garbage`,
		"trailing object":     `{"wakeOnAccess":true}{"wakeOnAccess":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(raw)
			require.Error(t, err)
			require.False(t, WakeAllowedOf(raw), "every parse failure must resolve to not-wakeable")
		})
	}
}

func TestWakeAllowedOfDefaultsAndOptOut(t *testing.T) {
	require.True(t, WakeAllowedOf(`{"idleTimeoutSeconds":300,"onIdle":"pause"}`), "wakeOnAccess defaults to true within a declared policy")
	require.True(t, WakeAllowedOf(`{}`), "an empty object is a declared wake-only policy")
	require.False(t, WakeAllowedOf(`{"wakeOnAccess":false}`))
	require.False(t, WakeAllowedOf(""), "missing annotation is never wakeable")
}
