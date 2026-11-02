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

package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
				Name:   "gitlab-token",
				Source: json.RawMessage(`{"type":"inline","value":"secret-token"}`),
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
	_, cleanup, err := credentialvault.StartActiveSocketServerRequestAware(srv.handleCredentialVaultActive, nil, socketPath, -1)
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
	initialTag := resp.Header.Get("ETag")
	require.NotEmpty(t, initialTag)
	require.Contains(t, string(body), "secret-token")
	require.Contains(t, string(body), "Private-Token")

	req, err := http.NewRequest(http.MethodGet, "http://credential-proxy/credential-vault/_active", nil)
	require.NoError(t, err)
	req.Header.Set("If-None-Match", initialTag)
	resp, err = client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotModified, resp.StatusCode)
	require.Equal(t, initialTag, resp.Header.Get("ETag"))

	_, err = store.Patch(credentialvault.MutationRequest{
		Credentials: &credentialvault.CredentialMutationSet{Replace: []credentialvault.Credential{{
			Name:   "gitlab-token",
			Source: json.RawMessage(`{"type":"inline","value":"new-secret-token"}`),
		}}},
	}, pol)
	require.NoError(t, err)
	req, err = http.NewRequest(http.MethodGet, "http://credential-proxy/credential-vault/_active", nil)
	require.NoError(t, err)
	req.Header.Set("If-None-Match", initialTag)
	resp, err = client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEqual(t, initialTag, resp.Header.Get("ETag"))
	require.Contains(t, string(body), "new-secret-token")
	require.NotContains(t, string(body), `"secret-token"`)

	req, err = http.NewRequest(http.MethodGet, "http://credential-proxy/credential-vault/_active", nil)
	require.NoError(t, err)
	req.Header.Set("If-None-Match", "not-quoted")
	resp, err = client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
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

type blockingVaultPolicyNft struct {
	stubNft
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func (n *blockingVaultPolicyNft) unblock() {
	n.releaseOnce.Do(func() { close(n.release) })
}

func (n *blockingVaultPolicyNft) ApplyStatic(ctx context.Context, p *policy.NetworkPolicy) error {
	n.calls++
	n.applied = p
	select {
	case n.entered <- struct{}{}:
	default:
	}
	select {
	case <-n.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestCredentialVaultCreateWaitsForPolicyMutationBarrier(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	initial := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	proxy := &stubProxy{updated: initial}
	nft := &blockingVaultPolicyNft{entered: make(chan struct{}, 1), release: make(chan struct{})}
	srv := &policyServer{
		proxy:           proxy,
		nft:             nft,
		enforcementMode: "dns+nft",
		credentialVault: credentialvault.NewStore(nil, func() bool { return true }),
	}
	t.Cleanup(nft.unblock)

	policyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodDelete, "/policy", strings.NewReader(`["code.example.com"]`))
		w := httptest.NewRecorder()
		srv.handlePolicy(w, req)
		policyDone <- w
	}()
	select {
	case <-nft.entered:
	case <-time.After(time.Second):
		t.Fatal("policy mutation did not reach nft apply")
	}

	vaultDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body := `{"credentials":[{"name":"gitlab-token","source":{"type":"inline","value":"secret-token"}}],"bindings":[{"name":"gitlab-api","match":{"hosts":["code.example.com"],"methods":["GET"],"paths":["/api/v8/*"]},"auth":{"type":"apiKey","name":"PRIVATE-TOKEN","credential":"gitlab-token"}}]}`
		req := httptest.NewRequest(http.MethodPost, "/credential-vault", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:4321"
		w := httptest.NewRecorder()
		srv.handleCredentialVault(w, req)
		vaultDone <- w
	}()

	select {
	case w := <-vaultDone:
		t.Fatalf("Vault create passed while policy apply was still in progress: status %d", w.Code)
	case <-time.After(100 * time.Millisecond):
	}

	nft.unblock()
	policyResponse := <-policyDone
	require.Equal(t, http.StatusOK, policyResponse.Code)
	vaultResponse := <-vaultDone
	require.Equal(t, http.StatusBadRequest, vaultResponse.Code)
	require.Contains(t, vaultResponse.Body.String(), "is not allowed by egress policy")
	state, err := srv.credentialVault.Sanitized()
	require.Error(t, err)
	require.Empty(t, state.Bindings)
}

func TestCredentialVaultMutationPanicReleasesPolicyMutationBarrier(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvMitmproxySslInsecure, "")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "")

	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			factoryCalled := make(chan struct{}, 1)
			registry := credentialvault.NewSourceRegistry()
			registry.Register("panic", func(json.RawMessage) (credentialvault.CredentialSource, error) {
				factoryCalled <- struct{}{}
				panic("credential source factory panic")
			})
			store := credentialvault.NewStoreWithRegistry(nil, func() bool { return true }, registry)
			if method == http.MethodPatch {
				_, err := store.Create(credentialvault.CreateRequest{Credentials: []credentialvault.Credential{{
					Name:   "gitlab-token",
					Source: json.RawMessage(`{"type":"inline","value":"initial-token"}`),
				}}}, nil)
				require.NoError(t, err)
			}

			srv := &policyServer{proxy: &stubProxy{}, credentialVault: store}
			httpServer := httptest.NewUnstartedServer(http.HandlerFunc(srv.handleCredentialVault))
			httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
			httpServer.Start()
			defer httpServer.Close()

			var body string
			if method == http.MethodPost {
				body = `{"credentials":[{"name":"gitlab-token","source":{"type":"panic"}}],"bindings":[]}`
			} else {
				body = `{"credentials":{"replace":[{"name":"gitlab-token","source":{"type":"panic"}}]}}`
			}
			req, err := http.NewRequest(method, httpServer.URL+"/credential-vault", strings.NewReader(body))
			require.NoError(t, err)
			resp, err := httpServer.Client().Do(req)
			var responseBody []byte
			if resp != nil {
				responseBody, _ = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			require.Error(t, err, "net/http should recover the panicking handler by closing the request connection")
			select {
			case <-factoryCalled:
			default:
				if resp == nil {
					t.Fatalf("credential source factory was not called; request error: %v", err)
				}
				t.Fatalf("credential source factory was not called; HTTP status %d body %q", resp.StatusCode, responseBody)
			}

			require.True(t, srv.mu.TryLock(), "policy mutation barrier should be released after the recovered panic")
			srv.mu.Unlock()
		})
	}
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
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
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
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	t.Setenv(constants.EnvCredentialVaultTrustedProxyCIDRs, "198.51.100.0/24")
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

func TestCredentialVaultWriteRejectsForwardedProtoFromUntrustedPeer(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	t.Setenv(constants.EnvCredentialVaultTrustedProxyCIDRs, "203.0.113.0/24")
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

	require.Equal(t, http.StatusUpgradeRequired, w.Result().StatusCode)
}

func TestCredentialVaultWriteSkipsTLSCheckByDefault(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
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

func TestCredentialVaultWriteRejectsDNSOnlyEnforcement(t *testing.T) {
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsOnly)
	initial := testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	srv := &policyServer{
		proxy:           &stubProxy{updated: initial},
		credentialVault: credentialvault.NewStore(nil, func() bool { return true }),
	}

	req := httptest.NewRequest(http.MethodPost, "/credential-vault", strings.NewReader(`{"credentials":[],"bindings":[]}`))
	req.RemoteAddr = "127.0.0.1:4321"
	w := httptest.NewRecorder()
	srv.handleCredentialVault(w, req)

	require.Equal(t, http.StatusPreconditionFailed, w.Result().StatusCode)
	require.Contains(t, w.Body.String(), "dns+nft")
}
