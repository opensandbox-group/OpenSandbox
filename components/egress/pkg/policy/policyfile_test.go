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

package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPolicyFileRoundTrip(t *testing.T) {
	body := json.RawMessage(`{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"}]}`)
	raw, err := EncodePolicyFile("runtime-1", "attachment-1", 7, body)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(raw), "\n"), "trailing newline, matching the policy file convention")

	pf, decoded, err := DecodePolicyFile(raw)
	require.NoError(t, err)
	assert.Equal(t, PolicyFileVersion, pf.Version)
	assert.Equal(t, "runtime-1", pf.Incarnation)
	assert.Equal(t, "attachment-1", pf.AttachmentID)
	assert.Equal(t, int64(7), pf.Revision)
	assert.JSONEq(t, string(body), string(decoded))

	// The decoded body parses to the same policy the caller encoded.
	pol, err := ParsePolicy(string(decoded))
	require.NoError(t, err)
	assert.Equal(t, "allow", pol.Evaluate("api.example.com"))
}

func TestPolicyFileEncodeValidation(t *testing.T) {
	body := json.RawMessage(`{}`)
	cases := []struct {
		name         string
		incarnation  string
		attachmentID string
		revision     int64
		body         json.RawMessage
	}{
		{"blank incarnation", " ", "a", 1, body},
		{"blank attachment", "r", " ", 1, body},
		{"zero revision", "r", "a", 0, body},
		{"negative revision", "r", "a", -1, body},
		{"invalid json body", "r", "a", 1, json.RawMessage(`{`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := EncodePolicyFile(tc.incarnation, tc.attachmentID, tc.revision, tc.body)
			assert.Error(t, err)
		})
	}
}

func TestPolicyFileDecodeValidation(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, _, err := DecodePolicyFile(nil)
		assert.ErrorContains(t, err, "empty")
	})
	t.Run("missing version", func(t *testing.T) {
		_, _, err := DecodePolicyFile([]byte(`{"incarnation":"r","attachmentId":"a","revision":1,"policy":{}}` + "\n"))
		assert.ErrorContains(t, err, "version")
	})
	t.Run("unknown fields rejected", func(t *testing.T) {
		_, _, err := DecodePolicyFile([]byte(`{"v":1,"incarnation":"r","attachmentId":"a","revision":1,"policy":{},"extra":true}` + "\n"))
		assert.Error(t, err)
	})
	t.Run("newer version is identifiable", func(t *testing.T) {
		_, _, err := DecodePolicyFile([]byte(`{"v":2,"incarnation":"r","attachmentId":"a","revision":1,"policy":{},"ct":"x"}` + "\n"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedPolicyFileVersion))
	})
	t.Run("missing body", func(t *testing.T) {
		_, _, err := DecodePolicyFile([]byte(`{"v":1,"incarnation":"r","attachmentId":"a","revision":1}` + "\n"))
		assert.ErrorContains(t, err, "body is missing")
	})
	t.Run("non-positive revision", func(t *testing.T) {
		_, _, err := DecodePolicyFile([]byte(`{"v":1,"incarnation":"r","attachmentId":"a","revision":0,"policy":{}}` + "\n"))
		assert.ErrorContains(t, err, "must be positive")
	})
}
