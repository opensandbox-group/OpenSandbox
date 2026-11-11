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
	"github.com/alibaba/opensandbox/egress/pkg/mitmpassthrough"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/revision"
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

// exchangeVaultJSON mirrors the vault_test.go exchange fixture (endpoint
// unix:///run/spi/spi-signer.sock) as a full-chain HTTP request body.
func exchangeVaultJSON() string {
	body, err := json.Marshal(credentialvault.CreateRequest{
		Credentials: []credentialvault.Credential{{
			Name:   "spi-signer",
			Source: json.RawMessage(`{"type":"exchange","endpoint":"unix:///run/spi/spi-signer.sock"}`),
		}},
		Bindings: []credentialvault.Binding{{
			Name: "internal-api",
			Match: credentialvault.Match{
				Hosts:   []string{"api.internal.example.com"},
				Methods: []string{"GET"},
				Paths:   []string{"/*"},
			},
			Auth: credentialvault.Auth{Credential: "spi-signer"},
		}},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func exchangeHandlerPolicy(t *testing.T) *policy.NetworkPolicy {
	t.Helper()
	return testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.internal.example.com"}]}`)
}

// newHandlerVaultFromEnv mirrors the production store wiring in
// startPolicyServer (policy_server.go): a plain store whose D13 resolver
// capability gate is derived from the operator env exactly once.
func newHandlerVaultFromEnv(t *testing.T) *credentialvault.Store {
	t.Helper()
	store := credentialvault.NewStore(nil, func() bool { return true })
	config, _ := credentialvault.ResolverConfigFromEnv()
	store.SetResolverConfig(config)
	return store
}

func newExchangeHandlerServer(t *testing.T) *policyServer {
	t.Helper()
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
	t.Setenv(constants.EnvMitmproxySslInsecure, "")
	return &policyServer{
		proxy:           &stubProxy{updated: exchangeHandlerPolicy(t)},
		credentialVault: newHandlerVaultFromEnv(t),
	}
}

func doVaultJSON(t *testing.T, srv *policyServer, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/credential-vault", nil)
	} else {
		req = httptest.NewRequest(method, "/credential-vault", strings.NewReader(body))
	}
	req.RemoteAddr = "127.0.0.1:4321"
	w := httptest.NewRecorder()
	srv.handleCredentialVault(w, req)
	return w
}

// OSEP-0025 T4: full-chain exchange create through the HTTP handler. The
// sanitized state must carry the typeless marker auth only, the exchange
// sourceType, and never the resolver endpoint.
func TestCredentialVaultExchangeCreateSanitizedShapeHandler(t *testing.T) {
	srv := newExchangeHandlerServer(t)

	created := doVaultJSON(t, srv, http.MethodPost, exchangeVaultJSON())
	require.Equal(t, http.StatusCreated, created.Code)
	createdBody := created.Body.String()
	require.Contains(t, createdBody, `"sourceType":"exchange"`)
	require.Contains(t, createdBody, `"credential":"spi-signer"`)
	require.NotContains(t, createdBody, "unix:///run/spi")
	require.NotContains(t, createdBody, `"type":""`)

	var state struct {
		Credentials []map[string]any `json:"credentials"`
		Bindings    []struct {
			Auth map[string]any `json:"auth"`
		} `json:"bindings"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &state))
	require.Len(t, state.Credentials, 1)
	require.Equal(t, "spi-signer", state.Credentials[0]["name"])
	require.Equal(t, "exchange", state.Credentials[0]["sourceType"])
	require.Equal(t, float64(1), state.Credentials[0]["revision"])
	require.Len(t, state.Bindings, 1)
	require.Equal(t, map[string]any{"credential": "spi-signer"}, state.Bindings[0].Auth)

	read := doVaultJSON(t, srv, http.MethodGet, "")
	require.Equal(t, http.StatusOK, read.Code)
	require.Contains(t, read.Body.String(), `"sourceType":"exchange"`)
	require.NotContains(t, read.Body.String(), "unix:///run/spi")

	credentialReq := httptest.NewRequest(http.MethodGet, "/credential-vault/credentials/spi-signer", nil)
	credentialReq.RemoteAddr = "127.0.0.1:4321"
	credentialW := httptest.NewRecorder()
	srv.handleCredentialVaultSubresource(credentialW, credentialReq)
	require.Equal(t, http.StatusOK, credentialW.Code)
	require.JSONEq(t, `{"name":"spi-signer","sourceType":"exchange","revision":1}`, credentialW.Body.String())

	bindingReq := httptest.NewRequest(http.MethodGet, "/credential-vault/bindings/internal-api", nil)
	bindingReq.RemoteAddr = "127.0.0.1:4321"
	bindingW := httptest.NewRecorder()
	srv.handleCredentialVaultSubresource(bindingW, bindingReq)
	require.Equal(t, http.StatusOK, bindingW.Code)
	require.Contains(t, bindingW.Body.String(), `"auth":{"credential":"spi-signer"}`)
	require.NotContains(t, bindingW.Body.String(), "unix:///run/spi")
}

func TestCredentialVaultExchangeCrossLockHandler(t *testing.T) {
	tests := []struct {
		name   string
		auth   credentialvault.Auth
		phrase string
	}{
		{
			name:   "static bearer references exchange credential",
			auth:   credentialvault.Auth{Type: "bearer", Credential: "spi-signer"},
			phrase: "only the typeless auth form may use exchange credentials",
		},
		{
			name:   "typeless references inline credential",
			auth:   credentialvault.Auth{Credential: "gitlab-token"},
			phrase: "typeless auth requires an exchange credential",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newExchangeHandlerServer(t)
			body, err := json.Marshal(credentialvault.CreateRequest{
				Credentials: []credentialvault.Credential{
					{Name: "gitlab-token", Source: json.RawMessage(`{"type":"inline","value":"secret-token"}`)},
					{Name: "spi-signer", Source: json.RawMessage(`{"type":"exchange","endpoint":"unix:///run/spi/spi-signer.sock"}`)},
				},
				Bindings: []credentialvault.Binding{{
					Name:  "mixed",
					Match: credentialvault.Match{Hosts: []string{"api.internal.example.com"}, Methods: []string{"GET"}},
					Auth:  tc.auth,
				}},
			})
			require.NoError(t, err)

			response := doVaultJSON(t, srv, http.MethodPost, string(body))
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Contains(t, response.Body.String(), tc.phrase)
			require.NotContains(t, response.Body.String(), "secret-token")
			require.NotContains(t, response.Body.String(), "unix:///run/spi")
		})
	}
}

// OSEP-0025 T4: 409 expectedRevision and 412 readiness semantics keep their
// fixed vocabulary for exchange vaults.
func TestCredentialVaultExchangeConflictAndPreconditionHandler(t *testing.T) {
	srv := newExchangeHandlerServer(t)
	created := doVaultJSON(t, srv, http.MethodPost, exchangeVaultJSON())
	require.Equal(t, http.StatusCreated, created.Code)

	stale := int64(99)
	staleBody, err := json.Marshal(credentialvault.MutationRequest{
		ExpectedRevision: &stale,
		Credentials: &credentialvault.CredentialMutationSet{Replace: []credentialvault.Credential{{
			Name:   "spi-signer",
			Source: json.RawMessage(`{"type":"exchange","endpoint":"unix:///run/spi/spi-signer-v2.sock"}`),
		}}},
	})
	require.NoError(t, err)
	conflict := doVaultJSON(t, srv, http.MethodPatch, string(staleBody))
	require.Equal(t, http.StatusConflict, conflict.Code)
	require.Contains(t, conflict.Body.String(), "credential vault revision conflict")

	current := int64(1)
	currentBody, err := json.Marshal(credentialvault.MutationRequest{
		ExpectedRevision: &current,
		Credentials: &credentialvault.CredentialMutationSet{Replace: []credentialvault.Credential{{
			Name:   "spi-signer",
			Source: json.RawMessage(`{"type":"exchange","endpoint":"unix:///run/spi/spi-signer-v2.sock"}`),
		}}},
	})
	require.NoError(t, err)
	rotated := doVaultJSON(t, srv, http.MethodPatch, string(currentBody))
	require.Equal(t, http.StatusOK, rotated.Code)
	require.NotContains(t, rotated.Body.String(), "unix:///run/spi")

	// 412 readiness gate: exchange vaults face the same preconditions as
	// inline vaults; insecure upstream TLS mode blocks writes.
	t.Setenv(constants.EnvMitmproxySslInsecure, "true")
	blocked := doVaultJSON(t, srv, http.MethodPatch, string(currentBody))
	require.Equal(t, http.StatusPreconditionFailed, blocked.Code)
	require.Contains(t, blocked.Body.String(), "insecure upstream TLS")
}

// OSEP-0025 T4: the D13 capability gate must bite through the HTTP handler
// when the operator resolver env is not ready, and pass once it is.
func TestCredentialVaultExchangeCapabilityGateHandler(t *testing.T) {
	t.Setenv(constants.EnvCredentialResolverTimeout, "0")
	srv := newExchangeHandlerServer(t)

	gated := doVaultJSON(t, srv, http.MethodPost, exchangeVaultJSON())
	require.Equal(t, http.StatusBadRequest, gated.Code)
	require.Contains(t, gated.Body.String(), "exchange credentials are not supported until the credential resolver operator configuration is ready")
	require.NotContains(t, gated.Body.String(), "unix:///run/spi")

	// Inline-only vaults are unaffected by the unready resolver env.
	inline, err := json.Marshal(testCredentialVaultRequest())
	require.NoError(t, err)
	srv.proxy = &stubProxy{updated: testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)}
	inlineCreated := doVaultJSON(t, srv, http.MethodPost, string(inline))
	require.Equal(t, http.StatusCreated, inlineCreated.Code)

	// PATCH introducing an exchange credential faces the same gate.
	patchBody, err := json.Marshal(credentialvault.MutationRequest{
		Credentials: &credentialvault.CredentialMutationSet{Add: []credentialvault.Credential{{
			Name:   "spi-signer",
			Source: json.RawMessage(`{"type":"exchange","endpoint":"unix:///run/spi/spi-signer.sock"}`),
		}}},
	})
	require.NoError(t, err)
	patchGated := doVaultJSON(t, srv, http.MethodPatch, string(patchBody))
	require.Equal(t, http.StatusBadRequest, patchGated.Code)
	require.Contains(t, patchGated.Body.String(), "credential resolver operator configuration")

	// Once the operator env parses, the same create passes.
	t.Setenv(constants.EnvCredentialResolverTimeout, "5")
	ready := newExchangeHandlerServer(t)
	passed := doVaultJSON(t, ready, http.MethodPost, exchangeVaultJSON())
	require.Equal(t, http.StatusCreated, passed.Code)
}

// OSEP-0025 T4: a mixed vault (static inline + exchange binding) keeps the
// static shape byte-for-byte while the exchange binding renders the marker.
func TestCredentialVaultMixedStaticAndExchangeHandler(t *testing.T) {
	srv := newExchangeHandlerServer(t)
	srv.proxy = &stubProxy{updated: testCredentialVaultPolicy(t, `{"defaultAction":"deny","egress":[
		{"action":"allow","target":"api.internal.example.com"},
		{"action":"allow","target":"code.example.com"}
	]}`)}
	body := exchangeVaultJSON()
	var request credentialvault.CreateRequest
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	request.Credentials = append(request.Credentials, credentialvault.Credential{
		Name:   "gitlab-token",
		Source: json.RawMessage(`{"type":"inline","value":"secret-token"}`),
	})
	request.Bindings = append(request.Bindings, credentialvault.Binding{
		Name:  "gitlab-api",
		Match: credentialvault.Match{Hosts: []string{"code.example.com"}, Methods: []string{"GET"}, Paths: []string{"/api/v8/*"}},
		Auth:  credentialvault.Auth{Type: "apiKey", Name: "PRIVATE-TOKEN", Credential: "gitlab-token"},
	})
	mixed, err := json.Marshal(request)
	require.NoError(t, err)

	created := doVaultJSON(t, srv, http.MethodPost, string(mixed))
	require.Equal(t, http.StatusCreated, created.Code)
	createdBody := created.Body.String()
	require.Contains(t, createdBody, `"sourceType":"exchange"`)
	require.Contains(t, createdBody, `"sourceType":"inline"`)
	require.Contains(t, createdBody, `"auth":{"type":"apiKey","name":"Private-Token"}`)
	require.Contains(t, createdBody, `"auth":{"credential":"spi-signer"}`)
	require.NotContains(t, createdBody, "unix:///run/spi")
}

// OSEP-0025 T4: the live runtime (NewLiveStore, selected in startPolicyServer
// when EnvExperimentalRevisionRuntime is truthy) rejects exchange creates with
// the fixed vocabulary even with a ready resolver configuration; the live
// write handler sanitizes every prepare failure to its fixed boundary, and
// inline-only vaults keep working.
func TestCredentialVaultLiveRevisionRuntimeRejectsExchangeHandler(t *testing.T) {
	liveWriteFixtureEnv(t)
	gate := mitmproxy.NewHealthGate()
	gate.SetReady(true)
	store := credentialvault.NewLiveStore(gate, func() bool { return true }, mitmpassthrough.Config{})
	config, configErr := credentialvault.ResolverConfigFromEnv()
	require.NoError(t, configErr)
	require.True(t, config.Ready())
	store.SetResolverConfig(config)

	// NewLiveStore admission: exchange is rejected even with ready resolver env.
	exchangeRequest := credentialvault.CreateRequest{
		Credentials: []credentialvault.Credential{{
			Name:   "spi-signer",
			Source: json.RawMessage(`{"type":"exchange","endpoint":"unix:///run/spi/spi-signer.sock"}`),
		}},
		Bindings: []credentialvault.Binding{{
			Name:  "internal-api",
			Match: credentialvault.Match{Hosts: []string{"api.internal.example.com"}, Methods: []string{"GET"}},
			Auth:  credentialvault.Auth{Credential: "spi-signer"},
		}},
	}
	_, err := store.Create(exchangeRequest, exchangeHandlerPolicy(t))
	require.ErrorContains(t, err, "exchange credentials are not supported in the live revision runtime")
	_, err = store.PrepareCreate(exchangeRequest, exchangeHandlerPolicy(t))
	require.ErrorContains(t, err, "exchange credentials are not supported in the live revision runtime")

	// Handler wiring: the live write path sanitizes every prepare failure to
	// its fixed vocabulary — including the exchange admission rejection.
	s := &policyServer{proxy: &stubProxy{updated: policy.DefaultDenyPolicy()}, mitmGate: gate, credentialVault: store}
	require.NoError(t, s.initRevisionRecoveryLocked(effectivePolicyInputs{user: s.proxy.CurrentPolicy()}))
	session := &mutationTestSession{fakeRevisionProcessSession: fakeRevisionProcessSession{config: validFakeRevisionIPCConfig()}}
	session.update = func(_ context.Context, snapshot credentialvault.ActiveSnapshot, epoch int64) (revision.Identity, error) {
		return mutationIdentity(t, snapshot, epoch), nil
	}
	s.mitm = &mitmTransparent{running: &mitmproxy.Running{}, revisionSession: session, currentGen: 7, revisionOwner: &revisionLaunchOwner{stop: func(*mitmproxy.Running) { t.Error("unexpected stop") }}}

	rejected := liveWriteDo(t, s, http.MethodPost, exchangeVaultJSON())
	require.Equal(t, http.StatusBadRequest, rejected.Code)
	require.Contains(t, rejected.Body.String(), "invalid credential vault mutation")
	require.NotContains(t, rejected.Body.String(), "unix:///run/spi")
	require.Zero(t, session.updateCalls)
	_, err = store.Sanitized()
	require.ErrorIs(t, err, credentialvault.ErrNotFound)

	// The live runtime still admits inline-only vaults unchanged.
	setMutationFixturePolicy(t, s, liveWritePolicy(t, true))
	accepted := liveWriteDo(t, s, http.MethodPost, liveWriteCreateBody())
	require.Equal(t, http.StatusCreated, accepted.Code)
	require.Equal(t, 1, session.updateCalls)
}

// liveWriteFixtureEnv isolates the live-runtime env without importing the
// revision-write fixture (which builds a plain, non-live store).
func liveWriteFixtureEnv(t *testing.T) {
	t.Helper()
	t.Setenv(constants.EnvExperimentalRevisionRuntime, "true")
	t.Setenv(constants.EnvMitmproxyTransparent, "true")
	t.Setenv(constants.EnvEgressMode, constants.PolicyDnsNft)
}
