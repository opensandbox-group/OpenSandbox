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

package routescope

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVerifyCrossLanguageVector(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	scope, err := verifier.Verify("f1.dGVuYW50LWE.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AQ")
	require.NoError(t, err)
	require.Equal(t, Scope{Namespace: "tenant-a", SandboxID: "sandbox-123", Port: 44772}, scope)
}

func TestVerifyRejectsTamperedNamespace(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	_, err := verifier.Verify("f1.dGVuYW50LWI.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AQ")
	require.ErrorIs(t, err, ErrUnauthorized)
}

func TestVerifyRejectsNonCanonicalBase64URL(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	for _, token := range []string{
		"f1.dGVuYW50LWE.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AR",
		"f1.dGVuYW50LWF.c2FuZGJveC0xMjM.44772.k.uo11HjECmnSuCCRF3v-1AQ",
	} {
		_, err := verifier.Verify(token)
		require.ErrorIs(t, err, ErrInvalidScope)
	}
}

func TestVerifyRejectsNonCanonicalPort(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	_, err := verifier.Verify("f1.dGVuYW50LWE.c2FuZGJveC0xMjM.+44772.k.uo11HjECmnSuCCRF3v-1AQ")
	require.ErrorIs(t, err, ErrInvalidScope)
}

func TestVerifyRejectsMalformedScope(t *testing.T) {
	verifier := &Verifier{Keys: map[string][]byte{"k": []byte("shared-secret")}}
	_, err := verifier.Verify("f1.bad")
	require.True(t, errors.Is(err, ErrInvalidScope))
}
