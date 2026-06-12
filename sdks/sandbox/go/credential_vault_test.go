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

package opensandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCreateCredentialVaultPayloadAndHeaders(t *testing.T) {
	_, client := newEgressServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/credential-vault", r.URL.Path)
		require.Equal(t, "test-egress-token", r.Header.Get(egressAuthHeader))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, map[string]any{
			"credentials": []any{
				map[string]any{
					"name": "api-token",
					"source": map[string]any{
						"type":  "inline",
						"value": "dummy-inline-value",
					},
				},
			},
			"bindings": []any{
				map[string]any{
					"name": "api-binding",
					"match": map[string]any{
						"schemes": []any{"https"},
						"ports":   []any{float64(443)},
						"hosts":   []any{"api.example.com"},
						"methods": []any{"GET"},
						"paths":   []any{"/v1/*"},
					},
					"auth": map[string]any{
						"type":       "apiKey",
						"name":       "X-Api-Key",
						"credential": "api-token",
					},
				},
			},
		}, body)

		jsonResponse(w, http.StatusCreated, CredentialVaultState{
			Revision: 1,
			Credentials: []CredentialMetadata{
				{Name: "api-token", SourceType: "inline", Revision: 1},
			},
			Bindings: []CredentialBindingMetadata{
				{
					Name:     "api-binding",
					Revision: 1,
					Match:    &CredentialMatch{Hosts: []string{"api.example.com"}},
					Auth:     &CredentialAuthMetadata{Type: "apiKey", Name: "X-Api-Key"},
				},
			},
		})
	})

	got, err := client.CreateCredentialVault(context.Background(), sampleCredentialVaultCreateRequest())
	require.NoErrorf(t, err, "CreateCredentialVault")
	require.Equal(t, 1, got.Revision)
	require.Len(t, got.Credentials, 1)
	require.Equal(t, "inline", got.Credentials[0].SourceType)
	require.Len(t, got.Bindings, 1)
	require.NotNil(t, got.Bindings[0].Auth)
	require.Equal(t, "apiKey", got.Bindings[0].Auth.Type)
}

func TestInlineCredentialSourceDefaultsTypeWhenMarshaled(t *testing.T) {
	body, err := json.Marshal(InlineCredentialSource{Value: "dummy-inline-value"})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, map[string]any{
		"type":  "inline",
		"value": "dummy-inline-value",
	}, got)
}

func TestPatchCredentialVaultPayload(t *testing.T) {
	expectedRevision := 3

	_, client := newEgressServer(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPatch, r.Method)
		require.Equal(t, "/credential-vault", r.URL.Path)

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, map[string]any{
			"expectedRevision": float64(3),
			"credentials": map[string]any{
				"add": []any{
					map[string]any{
						"name": "trace-token",
						"source": map[string]any{
							"type":  "inline",
							"value": "dummy-trace-value",
						},
					},
				},
				"replace": []any{
					map[string]any{
						"name": "api-token",
						"source": map[string]any{
							"type":  "inline",
							"value": "dummy-replacement-value",
						},
					},
				},
				"delete": []any{"old-token"},
			},
			"bindings": map[string]any{
				"add": []any{
					map[string]any{
						"name": "trace-binding",
						"match": map[string]any{
							"hosts": []any{"trace.example.com"},
						},
						"auth": map[string]any{
							"type": "customHeaders",
							"headers": []any{
								map[string]any{
									"name":       "X-Trace-Token",
									"credential": "trace-token",
								},
							},
						},
					},
				},
				"delete": []any{"old-binding"},
			},
		}, body)

		jsonResponse(w, http.StatusOK, CredentialVaultState{
			Revision:    4,
			Credentials: []CredentialMetadata{{Name: "api-token", SourceType: "inline", Revision: 4}},
			Bindings:    []CredentialBindingMetadata{{Name: "trace-binding", Revision: 1}},
		})
	})

	got, err := client.PatchCredentialVault(context.Background(), CredentialVaultPatchRequest{
		ExpectedRevision: &expectedRevision,
		Credentials: &CredentialMutationSet{
			Add: []Credential{
				{
					Name:   "trace-token",
					Source: InlineCredentialSource{Type: CredentialSourceInline, Value: "dummy-trace-value"},
				},
			},
			Replace: []Credential{
				{
					Name:   "api-token",
					Source: InlineCredentialSource{Type: CredentialSourceInline, Value: "dummy-replacement-value"},
				},
			},
			Delete: []string{"old-token"},
		},
		Bindings: &CredentialBindingMutationSet{
			Add: []CredentialBinding{
				{
					Name:  "trace-binding",
					Match: CredentialMatch{Hosts: []string{"trace.example.com"}},
					Auth: CredentialAuth{
						Type: CredentialAuthCustomHeaders,
						Headers: []CustomHeaderEntry{
							{Name: "X-Trace-Token", Credential: "trace-token"},
						},
					},
				},
			},
			Delete: []string{"old-binding"},
		},
	})
	require.NoErrorf(t, err, "PatchCredentialVault")
	require.Equal(t, 4, got.Revision)
}

func TestCredentialVaultGetListDeleteRoutes(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []string
	)

	_, client := newEgressServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestLine := r.Method + " " + r.URL.RequestURI()
		mu.Lock()
		seen = append(seen, requestLine)
		mu.Unlock()

		switch requestLine {
		case "GET /credential-vault":
			jsonResponse(w, http.StatusOK, CredentialVaultState{
				Revision:    7,
				Credentials: []CredentialMetadata{{Name: "api-token", SourceType: "inline", Revision: 2}},
				Bindings:    []CredentialBindingMetadata{{Name: "api-binding", Revision: 3}},
			})
		case "GET /credential-vault/credentials":
			jsonResponse(w, http.StatusOK, CredentialListResponse{
				Revision:    7,
				Credentials: []CredentialMetadata{{Name: "api-token", SourceType: "inline", Revision: 2}},
			})
		case "GET /credential-vault/credentials/api%2Ftoken%20one":
			jsonResponse(w, http.StatusOK, CredentialMetadata{
				Name:       "api/token one",
				SourceType: "inline",
				Revision:   2,
			})
		case "GET /credential-vault/bindings":
			jsonResponse(w, http.StatusOK, CredentialBindingListResponse{
				Revision: 7,
				Bindings: []CredentialBindingMetadata{
					{Name: "api-binding", Revision: 3},
				},
			})
		case "GET /credential-vault/bindings/api%2Fbinding%20one":
			jsonResponse(w, http.StatusOK, CredentialBindingMetadata{
				Name:     "api/binding one",
				Revision: 3,
			})
		case "DELETE /credential-vault":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s", requestLine)
		}
	})

	state, err := client.GetCredentialVault(context.Background())
	require.NoErrorf(t, err, "GetCredentialVault")
	require.Equal(t, 7, state.Revision)

	credentials, err := client.ListCredentialVaultCredentials(context.Background())
	require.NoErrorf(t, err, "ListCredentialVaultCredentials")
	require.Equal(t, 7, credentials.Revision)

	credential, err := client.GetCredentialVaultCredential(context.Background(), "api/token one")
	require.NoErrorf(t, err, "GetCredentialVaultCredential")
	require.Equal(t, "api/token one", credential.Name)

	bindings, err := client.ListCredentialVaultBindings(context.Background())
	require.NoErrorf(t, err, "ListCredentialVaultBindings")
	require.Equal(t, 7, bindings.Revision)

	binding, err := client.GetCredentialVaultBinding(context.Background(), "api/binding one")
	require.NoErrorf(t, err, "GetCredentialVaultBinding")
	require.Equal(t, "api/binding one", binding.Name)

	require.NoErrorf(t, client.DeleteCredentialVault(context.Background()), "DeleteCredentialVault")

	require.Equal(t, []string{
		"GET /credential-vault",
		"GET /credential-vault/credentials",
		"GET /credential-vault/credentials/api%2Ftoken%20one",
		"GET /credential-vault/bindings",
		"GET /credential-vault/bindings/api%2Fbinding%20one",
		"DELETE /credential-vault",
	}, seen)
}

func TestSandboxCredentialVaultForwardsEndpointHeaders(t *testing.T) {
	endpointHeaders := map[string]string{
		"OPENSANDBOX-EGRESS-AUTH": "egress-token-from-endpoint",
		"X-Route-Hint":            "credential-vault-vip",
	}

	egressSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, want := range endpointHeaders {
			if got := r.Header.Get(k); got != want {
				t.Fatalf("header %s = %q, want %q", k, got, want)
			}
		}
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/credential-vault", r.URL.Path)
		jsonResponse(w, http.StatusCreated, CredentialVaultState{Revision: 1})
	}))
	defer egressSrv.Close()

	lifecycleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/endpoints/") {
			jsonResponse(w, http.StatusOK, Endpoint{
				Endpoint: egressSrv.URL,
				Headers:  endpointHeaders,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer lifecycleSrv.Close()

	config := ConnectionConfig{Domain: lifecycleSrv.URL}
	sb := &Sandbox{
		id:        "sbx-credential-vault-headers",
		config:    &config,
		lifecycle: config.lifecycleClient(),
	}

	got, err := sb.CreateCredentialVault(context.Background(), sampleCredentialVaultCreateRequest())
	require.NoErrorf(t, err, "CreateCredentialVault")
	require.Equal(t, 1, got.Revision)
}

func TestCredentialVaultStateDoesNotRetainPlaintextSecretFields(t *testing.T) {
	var got CredentialVaultState
	require.NoError(t, json.Unmarshal([]byte(`{
		"revision": 7,
		"credentials": [
			{
				"name": "api-token",
				"sourceType": "inline",
				"revision": 2,
				"source": {"type": "inline", "value": "dummy-inline-value"},
				"value": "dummy-inline-value"
			}
		],
		"bindings": [
			{
				"name": "api-binding",
				"revision": 3,
				"match": {"hosts": ["api.example.com"]},
				"auth": {
					"type": "bearer",
					"name": "Authorization",
					"credential": "api-token",
					"headers": [{"name": "X-Token", "credential": "api-token"}]
				}
			}
		]
	}`), &got))
	require.Len(t, got.Credentials, 1)
	require.Len(t, got.Bindings, 1)
	require.NotNil(t, got.Bindings[0].Auth)

	data, err := json.Marshal(got)
	require.NoError(t, err)
	encoded := string(data)
	for _, forbidden := range []string{
		"dummy-inline-value",
		`"source"`,
		`"value"`,
		`"headers"`,
		`"credential":`,
	} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("sanitized state retained forbidden field/value %q in %s", forbidden, encoded)
		}
	}
}

func TestCreateSandboxRequestIncludesCredentialProxy(t *testing.T) {
	data, err := json.Marshal(CreateSandboxRequest{
		Image:           &ImageSpec{URI: "python:3.12"},
		Entrypoint:      []string{"/bin/sh"},
		ResourceLimits:  ResourceLimits{"cpu": "500m"},
		CredentialProxy: &CredentialProxyConfig{Enabled: true},
	})
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(data, &body))
	require.Equal(t, map[string]any{"enabled": true}, body["credentialProxy"])

	var req CreateSandboxRequest
	require.NoError(t, json.Unmarshal(data, &req))
	require.NotNil(t, req.CredentialProxy)
	require.Equal(t, true, req.CredentialProxy.Enabled)
}

func sampleCredentialVaultCreateRequest() CredentialVaultCreateRequest {
	return CredentialVaultCreateRequest{
		Credentials: []Credential{
			{
				Name: "api-token",
				Source: InlineCredentialSource{
					Type:  CredentialSourceInline,
					Value: "dummy-inline-value",
				},
			},
		},
		Bindings: []CredentialBinding{
			{
				Name: "api-binding",
				Match: CredentialMatch{
					Schemes: []CredentialScheme{CredentialSchemeHTTPS},
					Ports:   []int{443},
					Hosts:   []string{"api.example.com"},
					Methods: []string{"GET"},
					Paths:   []string{"/v1/*"},
				},
				Auth: CredentialAuth{
					Type:       CredentialAuthAPIKey,
					Name:       "X-Api-Key",
					Credential: "api-token",
				},
			},
		},
	}
}
