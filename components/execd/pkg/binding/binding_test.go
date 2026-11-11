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

package binding

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplyAndCurrent(t *testing.T) {
	previous := Apply(nil)
	defer Apply(previous)

	require.Nil(t, Current())

	applied := Apply(&RuntimeBinding{SandboxID: "sandbox-1", Generation: 7})
	require.Nil(t, applied, "first apply returns no previous binding")
	require.NotNil(t, Current())

	current := Current()
	require.NotNil(t, current)
	require.Equal(t, "sandbox-1", current.SandboxID)
	require.EqualValues(t, 7, current.Generation)

	second := Apply(&RuntimeBinding{SandboxID: "sandbox-2", Generation: 9})
	require.NotNil(t, second)
	require.Equal(t, "sandbox-1", second.SandboxID, "apply returns the previous binding")
	require.Equal(t, "sandbox-2", Current().SandboxID)
}

func TestAccessTokenHashRoundTrip(t *testing.T) {
	raw := "super-secret-token"
	hashed := HashAccessToken(raw)

	digest, err := ParseAccessTokenHash(hashed)
	require.NoError(t, err)

	b := &RuntimeBinding{AccessTokenHash: digest, HasAccessToken: true}
	require.True(t, b.VerifyAccessToken(raw))
	require.False(t, b.VerifyAccessToken("wrong-token"))
	require.False(t, b.VerifyAccessToken(""), "empty token must not verify")
}

func TestVerifyAccessTokenWithoutHash(t *testing.T) {
	b := &RuntimeBinding{SandboxID: "sandbox-1"}
	require.False(t, b.VerifyAccessToken("anything"))
	require.False(t, (*RuntimeBinding)(nil).VerifyAccessToken("anything"))
}

func TestParseAccessTokenHashRejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"missing prefix", "deadbeef"},
		{"not hex", "sha256:zzzz"},
		{"too short", "sha256:deadbeef"},
		{"empty", "sha256:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseAccessTokenHash(tt.raw)
			require.Error(t, err)
		})
	}

	// A valid 32-byte digest parses.
	_, err := ParseAccessTokenHash(HashAccessToken("x"))
	require.NoError(t, err)
}
