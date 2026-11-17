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
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCredentialVaultDomainPortUsesBindingScheme(t *testing.T) {
	t.Run("https permission renders the binding", func(t *testing.T) {
		store := NewStore(nil, func() bool { return true })
		pol := testCredentialPolicy(t, `{"egress":[{"action":"allow","target":"code.example.com","ports":[443]}]}`)
		_, err := store.Create(testCredentialVaultRequest(), pol)
		require.NoError(t, err)
		snapshot, err := store.ActiveSnapshot()
		require.NoError(t, err)
		require.Equal(t, []InjectionHeader{{Name: "Private-Token", Value: "secret-token"}}, snapshot.Bindings[0].Headers)
		require.Equal(t, []string{"https"}, snapshot.Bindings[0].Match.Schemes)
	})
	t.Run("DNS permission on HTTP does not authorize HTTPS credentials", func(t *testing.T) {
		store := NewStore(nil, func() bool { return true })
		pol := testCredentialPolicy(t, `{"egress":[{"action":"allow","target":"code.example.com","ports":[80]}]}`)
		_, err := store.Create(testCredentialVaultRequest(), pol)
		require.ErrorContains(t, err, "not allowed by egress policy")
	})
	t.Run("port deny narrows a legacy allow", func(t *testing.T) {
		store := NewStore(nil, func() bool { return true })
		pol := testCredentialPolicy(t, `{"egress":[{"action":"allow","target":"code.example.com"},{"action":"deny","target":"code.example.com","ports":[443]}]}`)
		_, err := store.Create(testCredentialVaultRequest(), pol)
		require.ErrorContains(t, err, "not allowed by egress policy")
	})
	t.Run("one allowed hostname cannot authorize wildcard credentials", func(t *testing.T) {
		store := NewStore(nil, func() bool { return true })
		pol := testCredentialPolicy(t, `{"egress":[{"action":"allow","target":"code.example.com","ports":[443]}]}`)
		request := testCredentialVaultRequest()
		request.Bindings[0].Match.Hosts = []string{"*.example.com"}
		_, err := store.Create(request, pol)
		require.ErrorContains(t, err, "not allowed by egress policy")
	})
}
