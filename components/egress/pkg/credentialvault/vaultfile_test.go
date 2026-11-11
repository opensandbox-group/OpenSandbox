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

package credentialvault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Vault file format (OSEP-0022 "Vault Storage"): the versioned shared
// server↔egress persistence contract. The vectors in testdata/vaultfile pin
// the exact bytes both consumers must agree on.
// ---------------------------------------------------------------------------

func sampleVaultFileBody() VaultFileBody {
	return VaultFileBody{
		Credentials: []Credential{
			{Name: "tok", Source: json.RawMessage(`{"type":"inline","value":"sekret"}`)},
		},
		Bindings: []Binding{
			{
				Name:  "api",
				Match: Match{Hosts: []string{"api.example.com"}},
				Auth:  Auth{Type: "bearer", Credential: "tok"},
			},
		},
	}
}

// TestVaultFileConformanceVectors walks every vector in testdata/vaultfile:
// a v1-*.json file must decode to the documented envelope and re-encode to
// the exact same bytes (the canonical form is deterministic — this is the
// contract a second consumer, e.g. the server's snapshot importer, codes
// against); an invalid-*.json file must be rejected.
func TestVaultFileConformanceVectors(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("testdata", "vaultfile"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, entry := range entries {
		name := entry.Name()
		raw, err := os.ReadFile(filepath.Join("testdata", "vaultfile", name))
		require.NoError(t, err, name)

		if filepath.Ext(name) != ".json" {
			continue
		}
		if len(name) >= 8 && name[:8] == "invalid-" {
			_, _, err := DecodeVaultFile(raw)
			assert.Error(t, err, "%s must be rejected", name)
			continue
		}
		vf, body, err := DecodeVaultFile(raw)
		require.NoError(t, err, name)
		assert.Equal(t, VaultFileVersion, vf.Version, name)
		assert.NotEmpty(t, vf.Incarnation, name)
		assert.NotEmpty(t, vf.AttachmentID, name)
		assert.GreaterOrEqual(t, vf.Revision, int64(1), name)
		assert.Equal(t, VaultFileBody{
			Credentials: body.Credentials,
			Bindings:    body.Bindings,
		}, body, name)

		reencoded, err := EncodeVaultFile(vf.Incarnation, vf.AttachmentID, vf.Revision, body)
		require.NoError(t, err, name)
		assert.Equal(t, string(raw), string(reencoded), "%s must be the canonical encoding (byte-exact round trip)", name)
	}
}

func TestVaultFileRoundTrip(t *testing.T) {
	raw, err := EncodeVaultFile("runtime-7", "attachment-3", 42, sampleVaultFileBody())
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"v":1`)
	assert.Contains(t, string(raw), `"incarnation":"runtime-7"`)
	assert.Contains(t, string(raw), `"attachmentId":"attachment-3"`)
	assert.Contains(t, string(raw), `"revision":42`)
	assert.Contains(t, string(raw), `"sekret"`)

	vf, body, err := DecodeVaultFile(raw)
	require.NoError(t, err)
	assert.Equal(t, "runtime-7", vf.Incarnation)
	assert.Equal(t, "attachment-3", vf.AttachmentID)
	assert.Equal(t, int64(42), vf.Revision)
	require.Len(t, body.Credentials, 1)
	require.Len(t, body.Bindings, 1)
	assert.JSONEq(t, `{"type":"inline","value":"sekret"}`, string(body.Credentials[0].Source))
	assert.Equal(t, "api", body.Bindings[0].Name)
}

// TestVaultFileUnknownVersionIsIdentifiable: the rejection must distinguish
// "structurally corrupt" from "written by a newer consumer" — only the
// former may ever be quarantined by the loader. A newer envelope carries
// unknown fields (kid/alg/ct), so the version gate must run BEFORE the
// strict schema.
func TestVaultFileUnknownVersionIsIdentifiable(t *testing.T) {
	future, err := EncodeVaultFile("rt", "att", 1, VaultFileBody{})
	require.NoError(t, err)
	v2 := strings.Replace(string(future), `"v":1`, `"v":2,"kid":"k1","alg":"A256GCM","ct":"cGxhaW50ZXh0"`, 1)

	_, _, err = DecodeVaultFile([]byte(v2))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsupportedFileVersion)

	// A missing or zero version is malformed, not futuristic: corrupt.
	_, _, err = DecodeVaultFile([]byte(`{"incarnation":"rt","attachmentId":"att","revision":1,"vault":{}}`))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnsupportedFileVersion)

	// A structurally corrupt file is NOT a version error either.
	_, _, err = DecodeVaultFile([]byte("{corrupt"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnsupportedFileVersion)
}

func TestVaultFileDecodeRejects(t *testing.T) {
	valid, err := EncodeVaultFile("rt", "att", 1, VaultFileBody{})
	require.NoError(t, err)

	cases := map[string]string{
		"empty input":             "",
		"whitespace only":         "  \n ",
		"not json":                "{nope",
		"array envelope":          `[1,2,3]`,
		"missing version":         `{"incarnation":"rt","attachmentId":"att","revision":1,"vault":{}}`,
		"future version":          `{"v":2,"incarnation":"rt","attachmentId":"att","revision":1,"vault":{}}`,
		"version zero":            `{"v":0,"incarnation":"rt","attachmentId":"att","revision":1,"vault":{}}`,
		"blank incarnation":       `{"v":1,"incarnation":"  ","attachmentId":"att","revision":1,"vault":{}}`,
		"missing incarnation":     `{"v":1,"attachmentId":"att","revision":1,"vault":{}}`,
		"blank attachmentId":      `{"v":1,"incarnation":"rt","attachmentId":"","revision":1,"vault":{}}`,
		"missing attachmentId":    `{"v":1,"incarnation":"rt","revision":1,"vault":{}}`,
		"zero revision":           `{"v":1,"incarnation":"rt","attachmentId":"att","revision":0,"vault":{}}`,
		"negative revision":       `{"v":1,"incarnation":"rt","attachmentId":"att","revision":-3,"vault":{}}`,
		"missing vault body":      `{"v":1,"incarnation":"rt","attachmentId":"att","revision":1}`,
		"unknown envelope field":  `{"v":1,"incarnation":"rt","attachmentId":"att","revision":1,"vault":{},"extra":true}`,
		"unknown body field":      `{"v":1,"incarnation":"rt","attachmentId":"att","revision":1,"vault":{"credentials":[],"bindings":[],"extra":1}}`,
		"vault body not object":   `{"v":1,"incarnation":"rt","attachmentId":"att","revision":1,"vault":"nope"}`,
		"legacy incarnation-only": `{"v":1,"incarnation":"rt","revision":1,"vault":{"credentials":[],"bindings":[]}}`,
	}
	for name, input := range cases {
		_, _, err := DecodeVaultFile([]byte(input))
		assert.Error(t, err, name)
	}

	// The valid baseline decodes, so the rejections above are about the
	// mutation, not the shape itself.
	_, _, err = DecodeVaultFile(valid)
	assert.NoError(t, err)
}

func TestVaultFileEncodeRejects(t *testing.T) {
	_, err := EncodeVaultFile("  ", "att", 1, VaultFileBody{})
	assert.Error(t, err, "blank incarnation")
	_, err = EncodeVaultFile("", "att", 1, VaultFileBody{})
	assert.Error(t, err, "empty incarnation")
	_, err = EncodeVaultFile("rt", " ", 1, VaultFileBody{})
	assert.Error(t, err, "blank attachmentId")
	_, err = EncodeVaultFile("rt", "", 1, VaultFileBody{})
	assert.Error(t, err, "empty attachmentId")
	_, err = EncodeVaultFile("rt", "att", 0, VaultFileBody{})
	assert.Error(t, err, "zero revision")
	_, err = EncodeVaultFile("rt", "att", -1, VaultFileBody{})
	assert.Error(t, err, "negative revision")
}
