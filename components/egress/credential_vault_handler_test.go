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

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func testCredentialVaultPolicy(t *testing.T, raw string) *policy.NetworkPolicy {
	t.Helper()
	pol, err := policy.ParsePolicy(raw)
	require.NoError(t, err)
	return pol
}

func testCredentialVaultRequest() credentialvault.CreateRequest {
	return credentialvault.CreateRequest{
		Credentials: []credentialvault.Credential{
			{
				Name: "gitlab-token",
				Source: credentialvault.InlineCredentialSource{
					Type:  "inline",
					Value: "secret-token",
				},
			},
		},
		Bindings: []credentialvault.Binding{
			{
				Name: "gitlab-api",
				Match: credentialvault.Match{
					Hosts:   []string{"code.example.com"},
					Methods: []string{"GET"},
					Paths:   []string{"/api/v8/*"},
				},
				Auth: credentialvault.Auth{
					Type:       "apiKey",
					Name:       "PRIVATE-TOKEN",
					Credential: "gitlab-token",
				},
			},
		},
	}
}

func TestCredentialVaultActiveTCPAlwaysForbidden(t *testing.T) {
	store := credentialvault.NewStore(nil, func() bool { return true })
	pol := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)
	srv := &policyServer{
		token:           "public-egress-token",
		credentialVault: store,
	}

	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	req.Header.Set(constants.EgressAuthTokenHeader, "public-egress-token")
	w := httptest.NewRecorder()

	srv.handleCredentialVaultSubresource(w, req)

	require.Equal(t, http.StatusForbidden, w.Result().StatusCode)
	require.NotContains(t, w.Body.String(), "secret-token")
}

func TestCredentialVaultActiveUnixSocketReturnsSnapshot(t *testing.T) {
	store := credentialvault.NewStore(nil, func() bool { return true })
	pol := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)
	srv := &policyServer{
		token:           "public-egress-token",
		credentialVault: store,
	}

	tmpDir, err := os.MkdirTemp("/tmp", "opensandbox-active-*")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, os.RemoveAll(tmpDir))
	})
	socketPath := filepath.Join(tmpDir, "credential-proxy", "active.sock")
	_, cleanup, err := credentialvault.StartActiveSocketServer(srv.handleCredentialVaultActive, socketPath, -1)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, cleanup(ctx))
	})

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		},
	}
	resp, err := client.Get("http://credential-proxy/credential-vault/_active")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "secret-token")
	require.Contains(t, string(body), "Private-Token")
}

func TestCredentialVaultActiveBindingBlocksEgressPolicyRemoval(t *testing.T) {
	initial := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	store := credentialvault.NewStore(nil, func() bool { return true })
	_, err := store.Create(testCredentialVaultRequest(), initial)
	require.NoError(t, err)
	srv := &policyServer{
		proxy:           proxy,
		nft:             nft,
		enforcementMode: "dns+nft",
		credentialVault: store,
	}

	req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(`["code.example.com"]`))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)

	require.Equal(t, http.StatusBadRequest, w.Result().StatusCode)
	require.Len(t, proxy.updated.Egress, 1)
	require.Equal(t, 0, nft.calls)
}

func TestCredentialVaultActiveBindingBlocksPolicyReset(t *testing.T) {
	initial := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	proxy := &stubProxy{updated: initial}
	nft := &stubNft{}
	store := credentialvault.NewStore(nil, func() bool { return true })
	_, err := store.Create(testCredentialVaultRequest(), initial)
	require.NoError(t, err)
	srv := &policyServer{
		proxy:           proxy,
		nft:             nft,
		enforcementMode: "dns+nft",
		credentialVault: store,
	}

	req := httptest.NewRequest(http.MethodPost, "/policy", strings.NewReader(""))
	w := httptest.NewRecorder()
	srv.handlePolicy(w, req)

	require.Equal(t, http.StatusBadRequest, w.Result().StatusCode)
	require.Contains(t, w.Body.String(), "credential vault policy validation")
	require.Len(t, proxy.updated.Egress, 1)
	require.Equal(t, 0, nft.calls)
}

func TestCredentialVaultDeleteRequiresReady(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "")
	srv := &policyServer{
		credentialVault: credentialvault.NewStore(nil, func() bool { return true }),
	}

	req := httptest.NewRequest(http.MethodDelete, "/credential-vault", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	w := httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusPreconditionFailed, w.Result().StatusCode)
	require.Contains(t, w.Body.String(), "transparent mitmproxy")
}

func TestCredentialVaultWriteRequiresTLSOrLoopback(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	initial := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	srv := &policyServer{
		proxy:                     &stubProxy{updated: initial},
		credentialVault:           credentialvault.NewStore(nil, func() bool { return true }),
		credentialVaultRequireTLS: true,
	}

	req := httptest.NewRequest(http.MethodPost, "/credential-vault", strings.NewReader(`{"credentials":[],"bindings":[]}`))
	req.RemoteAddr = "198.51.100.10:1234"
	w := httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusUpgradeRequired, w.Result().StatusCode)

	req = httptest.NewRequest(http.MethodPost, "/credential-vault", strings.NewReader(`{"credentials":[],"bindings":[]}`))
	req.RemoteAddr = "127.0.0.1:4321"
	w = httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusCreated, w.Result().StatusCode)

	req = httptest.NewRequest(http.MethodDelete, "/credential-vault", nil)
	req.RemoteAddr = "198.51.100.10:1234"
	w = httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusUpgradeRequired, w.Result().StatusCode)

	req = httptest.NewRequest(http.MethodDelete, "/credential-vault", nil)
	req.RemoteAddr = "127.0.0.1:4321"
	w = httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusNoContent, w.Result().StatusCode)
}

func TestCredentialVaultWriteAllowsForwardedProto(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	initial := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	srv := &policyServer{
		proxy:                     &stubProxy{updated: initial},
		credentialVault:           credentialvault.NewStore(nil, func() bool { return true }),
		credentialVaultRequireTLS: true,
	}

	req := httptest.NewRequest(http.MethodPost, "/credential-vault", strings.NewReader(`{"credentials":[],"bindings":[]}`))
	req.RemoteAddr = "198.51.100.10:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusCreated, w.Result().StatusCode)
}

func TestCredentialVaultWriteSkipsTLSCheckByDefault(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	initial := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		credentialVault: credentialvault.NewStore(nil, func() bool { return true }),
	}

	req := httptest.NewRequest(http.MethodPost, "/credential-vault", strings.NewReader(`{"credentials":[],"bindings":[]}`))
	req.RemoteAddr = "198.51.100.10:1234"
	w := httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusCreated, w.Result().StatusCode)
}
