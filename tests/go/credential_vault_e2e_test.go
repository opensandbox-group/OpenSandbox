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

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

const credentialVaultDefaultTargetHost = "credential-vault-e2e.opensandbox.test"

var credentialVaultSecrets = map[string]string{
	"bearer-token":           "vault-bearer-token",
	"basic-token":            "dXNlcjpwYXNz",
	"api-key-token":          "vault-api-key-token",
	"client-id":              "vault-client-id",
	"client-secret":          "vault-client-secret",
	"runtime-token":          "vault-runtime-token",
	"runtime-token-replaced": "vault-runtime-token-replaced",
}

func TestCredentialVaultInjectsAllAuthTypes(t *testing.T) {
	targetIP := credentialVaultTargetIP(t)
	ctx, sb := createCredentialVaultSandbox(t, targetIP)

	state, err := sb.CreateCredentialVault(ctx, opensandbox.CredentialVaultCreateRequest{
		Credentials: credentialVaultCredentials(
			"bearer-token",
			"basic-token",
			"api-key-token",
			"client-id",
			"client-secret",
			"runtime-token",
			"runtime-token-replaced",
		),
		Bindings: []opensandbox.CredentialBinding{
			credentialVaultBinding("bearer", "/bearer", opensandbox.CredentialAuth{
				Type:       opensandbox.CredentialAuthBearer,
				Credential: "bearer-token",
			}),
			credentialVaultBinding("basic", "/basic", opensandbox.CredentialAuth{
				Type:       opensandbox.CredentialAuthBasic,
				Credential: "basic-token",
			}),
			credentialVaultBinding("api-key", "/api-key", opensandbox.CredentialAuth{
				Type:       opensandbox.CredentialAuthAPIKey,
				Name:       "X-Api-Key",
				Credential: "api-key-token",
			}),
			credentialVaultBinding("custom-headers", "/custom-headers", opensandbox.CredentialAuth{
				Type: opensandbox.CredentialAuthCustomHeaders,
				Headers: []opensandbox.CustomHeaderEntry{
					{Name: "X-Client-Id", Credential: "client-id"},
					{Name: "X-Client-Secret", Credential: "client-secret"},
				},
			}),
		},
	})
	require.NoError(t, err)

	statePayload, err := json.Marshal(state)
	require.NoError(t, err)
	for _, secret := range credentialVaultSecrets {
		require.NotContains(t, string(statePayload), secret)
	}

	gotAuthTypes := map[string]bool{}
	for _, binding := range state.Bindings {
		if binding.Auth != nil {
			gotAuthTypes[binding.Auth.Type] = true
		}
	}
	require.Equal(t, map[string]bool{
		"bearer":        true,
		"basic":         true,
		"apiKey":        true,
		"customHeaders": true,
	}, gotAuthTypes)

	for _, path := range []string{"/bearer", "/basic", "/api-key", "/custom-headers"} {
		response := credentialVaultCurlJSON(t, ctx, sb, targetIP, path, true)
		require.Equal(t, true, response["ok"])
		require.Equal(t, path[1:], response["case"])
		require.Empty(t, stringSliceFromJSON(t, response["missingOrInvalid"]))
	}
}

func TestCredentialVaultRuntimeMutationAddsReplacesAndDeletesBinding(t *testing.T) {
	targetIP := credentialVaultTargetIP(t)
	ctx, sb := createCredentialVaultSandbox(t, targetIP)

	state, err := sb.CreateCredentialVault(ctx, opensandbox.CredentialVaultCreateRequest{})
	require.NoError(t, err)
	require.Equal(t, 1, state.Revision)
	require.Empty(t, state.Credentials)
	require.Empty(t, state.Bindings)

	state, err = sb.PatchCredentialVault(ctx, opensandbox.CredentialVaultPatchRequest{
		ExpectedRevision: intPtr(state.Revision),
		Credentials: &opensandbox.CredentialMutationSet{
			Add: credentialVaultCredentials("runtime-token"),
		},
		Bindings: &opensandbox.CredentialBindingMutationSet{
			Add: []opensandbox.CredentialBinding{
				credentialVaultBinding("runtime-added", "/runtime-added", opensandbox.CredentialAuth{
					Type:       opensandbox.CredentialAuthAPIKey,
					Name:       "X-Runtime-Token",
					Credential: "runtime-token",
				}),
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 2, state.Revision)
	require.Len(t, state.Credentials, 1)
	require.Equal(t, "runtime-token", state.Credentials[0].Name)
	require.Len(t, state.Bindings, 1)
	require.Equal(t, "runtime-added", state.Bindings[0].Name)

	response := credentialVaultCurlJSON(t, ctx, sb, targetIP, "/runtime-added", true)
	require.Equal(t, true, response["ok"])
	require.Equal(t, "runtime-added", response["case"])
	require.Empty(t, stringSliceFromJSON(t, response["missingOrInvalid"]))

	state, err = sb.PatchCredentialVault(ctx, opensandbox.CredentialVaultPatchRequest{
		ExpectedRevision: intPtr(state.Revision),
		Bindings:         &opensandbox.CredentialBindingMutationSet{Delete: []string{"runtime-added"}},
	})
	require.NoError(t, err)
	require.Equal(t, 3, state.Revision)
	require.Empty(t, state.Bindings)

	state, err = sb.PatchCredentialVault(ctx, opensandbox.CredentialVaultPatchRequest{
		ExpectedRevision: intPtr(state.Revision),
		Credentials: &opensandbox.CredentialMutationSet{
			Replace: []opensandbox.Credential{
				credentialVaultCredential("runtime-token", "runtime-token-replaced"),
			},
		},
		Bindings: &opensandbox.CredentialBindingMutationSet{
			Add: []opensandbox.CredentialBinding{
				credentialVaultBinding("runtime-replaced", "/runtime-replaced", opensandbox.CredentialAuth{
					Type:       opensandbox.CredentialAuthAPIKey,
					Name:       "X-Runtime-Token",
					Credential: "runtime-token",
				}),
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, 4, state.Revision)
	require.Len(t, state.Credentials, 1)
	require.Equal(t, "runtime-token", state.Credentials[0].Name)
	require.Len(t, state.Bindings, 1)
	require.Equal(t, "runtime-replaced", state.Bindings[0].Name)

	statePayload, err := json.Marshal(state)
	require.NoError(t, err)
	require.NotContains(t, string(statePayload), credentialVaultSecrets["runtime-token"])
	require.NotContains(t, string(statePayload), credentialVaultSecrets["runtime-token-replaced"])

	response = credentialVaultCurlJSON(t, ctx, sb, targetIP, "/runtime-replaced", true)
	require.Equal(t, true, response["ok"])
	require.Equal(t, "runtime-replaced", response["case"])
	require.Empty(t, stringSliceFromJSON(t, response["missingOrInvalid"]))

	response = credentialVaultCurlJSON(t, ctx, sb, targetIP, "/runtime-added", false)
	require.Equal(t, false, response["ok"])
	require.Equal(t, "runtime-added", response["case"])
	require.Equal(t, []string{"x-runtime-token"}, stringSliceFromJSON(t, response["missingOrInvalid"]))

	state, err = sb.PatchCredentialVault(ctx, opensandbox.CredentialVaultPatchRequest{
		ExpectedRevision: intPtr(state.Revision),
		Bindings:         &opensandbox.CredentialBindingMutationSet{Delete: []string{"runtime-replaced"}},
	})
	require.NoError(t, err)
	require.Equal(t, 5, state.Revision)
	require.Empty(t, state.Bindings)

	state, err = sb.PatchCredentialVault(ctx, opensandbox.CredentialVaultPatchRequest{
		ExpectedRevision: intPtr(state.Revision),
		Credentials:      &opensandbox.CredentialMutationSet{Delete: []string{"runtime-token"}},
	})
	require.NoError(t, err)
	require.Equal(t, 6, state.Revision)
	require.Empty(t, state.Credentials)
}

func credentialVaultTargetHost() string {
	if host := os.Getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST"); host != "" {
		return host
	}
	return credentialVaultDefaultTargetHost
}

func credentialVaultTargetIP(t *testing.T) string {
	t.Helper()
	targetIP := os.Getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP")
	if targetIP == "" {
		t.Skip("set OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP to run Credential Vault E2E")
	}
	return targetIP
}

func createCredentialVaultSandbox(t *testing.T, targetIP string) (context.Context, *opensandbox.Sandbox) {
	t.Helper()

	image := os.Getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_SANDBOX_IMAGE")
	if image == "" {
		image = getSandboxImage()
	}

	config := connectionConfigForStreaming(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image:          image,
		ResourceLimits: credentialVaultSandboxResource(),
		ReadyTimeout:   90 * time.Second,
		NetworkPolicy: &opensandbox.NetworkPolicy{
			DefaultAction: "deny",
			Egress: []opensandbox.NetworkRule{
				{Action: "allow", Target: credentialVaultTargetHost()},
				{Action: "allow", Target: targetIP},
			},
		},
		CredentialProxy: &opensandbox.CredentialProxyConfig{Enabled: true},
		Metadata: map[string]string{
			credentialVaultLabelKey(): credentialVaultLabelValue(),
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sb.Kill(context.Background()) })
	return ctx, sb
}

func credentialVaultSandboxResource() opensandbox.ResourceLimits {
	cpu := os.Getenv("OPENSANDBOX_E2E_SANDBOX_CPU")
	if cpu == "" {
		cpu = "1"
	}
	memory := os.Getenv("OPENSANDBOX_E2E_SANDBOX_MEMORY")
	if memory == "" {
		memory = "2Gi"
	}
	return opensandbox.ResourceLimits{"cpu": cpu, "memory": memory}
}

func credentialVaultCredentials(names ...string) []opensandbox.Credential {
	credentials := make([]opensandbox.Credential, 0, len(names))
	for _, name := range names {
		credentials = append(credentials, credentialVaultCredential(name, name))
	}
	return credentials
}

func credentialVaultCredential(name, valueName string) opensandbox.Credential {
	return opensandbox.Credential{
		Name: name,
		Source: opensandbox.InlineCredentialSource{
			Type:  opensandbox.CredentialSourceInline,
			Value: credentialVaultSecrets[valueName],
		},
	}
}

func credentialVaultBinding(name, path string, auth opensandbox.CredentialAuth) opensandbox.CredentialBinding {
	return opensandbox.CredentialBinding{
		Name: name,
		Match: opensandbox.CredentialMatch{
			Schemes: []opensandbox.CredentialScheme{opensandbox.CredentialSchemeHTTP},
			Ports:   []int{80},
			Hosts:   []string{credentialVaultTargetHost()},
			Methods: []string{"GET"},
			Paths:   []string{path},
		},
		Auth: auth,
	}
}

func credentialVaultCurlJSON(
	t *testing.T,
	ctx context.Context,
	sb *opensandbox.Sandbox,
	targetIP string,
	path string,
	failOnHTTPError bool,
) map[string]any {
	t.Helper()
	failFlag := ""
	if failOnHTTPError {
		failFlag = "--fail "
	}
	command := "curl " + failFlag +
		"--silent --show-error --connect-timeout 5 --max-time 20 " +
		"--resolve " + credentialVaultTargetHost() + ":80:" + targetIP + " " +
		"http://" + credentialVaultTargetHost() + path
	for _, secret := range credentialVaultSecrets {
		require.NotContains(t, command, secret)
	}

	exec, err := sb.RunCommand(ctx, command, nil)
	require.NoError(t, err)
	require.Nil(t, exec.Error)
	require.NotNil(t, exec.ExitCode)
	require.Equal(t, 0, *exec.ExitCode)

	stdout := exec.Text()
	require.NotEmpty(t, stdout)

	var response map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &response))
	return response
}

func stringSliceFromJSON(t *testing.T, value any) []string {
	t.Helper()
	values, ok := value.([]any)
	require.Truef(t, ok, "expected []any, got %T", value)
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		require.Truef(t, ok, "expected string, got %T", value)
		result = append(result, text)
	}
	return result
}

func credentialVaultLabelKey() string {
	if key := os.Getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY"); key != "" {
		return key
	}
	return "opensandbox.e2e"
}

func credentialVaultLabelValue() string {
	if value := os.Getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE"); value != "" {
		return value
	}
	return "credential-vault"
}

func intPtr(v int) *int {
	return &v
}
