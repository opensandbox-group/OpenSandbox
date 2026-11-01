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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/actionhandler"
	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// bindBodyWithName builds a SET_BINDING envelope whose SandboxRef.Name is the
// wire sandboxId recorded for the resolve path.
func bindBodyWithName(t *testing.T, uid, name, ip, runtimeID, attID string, specGen uint64, input *string) string {
	t.Helper()
	body := bindBody(t, uid, ip, runtimeID, attID, specGen, input)
	var env actionhandler.Envelope
	require.NoError(t, json.Unmarshal([]byte(body), &env))
	env.Sandbox.Name = name
	raw, err := json.Marshal(env)
	require.NoError(t, err)
	return string(raw)
}

const resolveTestPolicy = `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.internal.example.com"}]}`

// bindNamedAndReady drives a subject to active with a SET_BINDING whose
// SandboxRef.Name is the wire sandboxId the resolve path must use.
func bindNamedAndReady(t *testing.T, srv *fastSandboxPolicyServer, uid, name, ip, runtimeID, attID string, specGen uint64) subject.Subject {
	t.Helper()
	rec := doAction(t, srv, bindBodyWithName(t, uid, name, ip, runtimeID, attID, specGen, strPtr(resolveTestPolicy)))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, uid, runtimeID, attID, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	return subject.FromSandboxUID(uid)
}

// fakeResolverServer serves the egress→resolver contract over a real unix
// socket: deterministic headers keyed on the request parts (mirroring the
// tests/resolver stub) plus optional per-request delay.
type fakeResolverServer struct {
	listener net.Listener
	server   *httptest.Server
	requests chan resolveServerRequest
	delay    time.Duration
}

type resolveServerRequest struct {
	Subject    string
	Credential string
	Binding    string
	Authority  string
	Path       string
}

func startFakeResolverServer(t *testing.T, delay time.Duration) *fakeResolverServer {
	t.Helper()
	// Short temp dir: AF_UNIX sun_path is capped near 104 bytes and the
	// default testing temp dirs exceed that on macOS.
	dir, err := os.MkdirTemp("", "osbr-resolver-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", dir+"/resolver.sock")
	require.NoError(t, err)
	fake := &fakeResolverServer{listener: listener, requests: make(chan resolveServerRequest, 64), delay: delay}
	mux := http.NewServeMux()
	mux.HandleFunc("/resolve", func(w http.ResponseWriter, r *http.Request) {
		var document struct {
			Subject     string `json:"subject"`
			Credential  string `json:"credential"`
			Binding     string `json:"binding"`
			Destination struct {
				Scheme    string `json:"scheme"`
				Authority string `json:"authority"`
				SNI       string `json:"sni"`
			} `json:"destination"`
			Request struct {
				Method string `json:"method"`
				Host   string `json:"host"`
				Path   string `json:"path"`
			} `json:"request"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&document))
		fake.requests <- resolveServerRequest{
			Subject:    document.Subject,
			Credential: document.Credential,
			Binding:    document.Binding,
			Authority:  document.Destination.Authority,
			Path:       document.Request.Path,
		}
		if fake.delay > 0 {
			time.Sleep(fake.delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"headers":{"x-test-subject":%q,"authorization":"Bearer test-%s"},"redactions":{"authorization":["Bearer test-%s"]}}`,
			document.Subject, document.Credential, document.Credential)))
	})
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: mux}}
	server.Start()
	t.Cleanup(server.Close)
	fake.server = server
	return fake
}

func (f *fakeResolverServer) endpoint() string {
	return "unix://" + f.listener.Addr().String()
}

// seedExchangeVault installs an exchange binding on the subject's vault.
func seedExchangeVault(t *testing.T, srv *fastSandboxPolicyServer, s subject.Subject, reg *subject.MemoryRegistry, endpoint string) {
	t.Helper()
	vault := srv.vaultFor(s)
	require.NotNil(t, vault)
	_, err := vault.Create(credentialvault.CreateRequest{
		Credentials: []credentialvault.Credential{{
			Name:   "spi-signer",
			Source: json.RawMessage(`{"type":"exchange","endpoint":"` + endpoint + `"}`),
		}},
		Bindings: []credentialvault.Binding{{
			Name:  "internal-api",
			Match: credentialvault.Match{Hosts: []string{"api.internal.example.com"}, Methods: []string{"GET"}, Paths: []string{"/*"}},
			Auth:  credentialvault.Auth{Credential: "spi-signer"},
		}},
	}, reg.EffectivePolicy(s))
	require.NoError(t, err)
}

func fastSandboxResolveRequest(ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve?clientIp="+ip, strings.NewReader(
		`{"binding":"internal-api","sni":"api.internal.example.com",`+
			`"request":{"scheme":"https","host":"api.internal.example.com","port":443,"method":"GET","path":"/v1/foo"}}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// resolveHandlerFor builds the production resolve endpoint around the server.
func resolveHandlerFor(srv *fastSandboxPolicyServer) http.Handler {
	return credentialvault.NewResolveHandler(srv.resolveVaultAccess)
}

func TestFastSandboxResolveSubjectDerivation(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	fake := startFakeResolverServer(t, 0)

	// Wire subject = SandboxRef.Name (sandboxId), not the internal UID.
	const sandboxName = "fsb-wire-42"
	s := bindNamedAndReady(t, srv, "uid-1", sandboxName, testIP, testFenceR, testFenceA, 1)

	seedExchangeVault(t, srv, s, reg, fake.endpoint())

	handler := resolveHandlerFor(srv)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, fastSandboxResolveRequest(testIP))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	served := <-fake.requests
	require.Equal(t, sandboxName, served.Subject, "wire subject must be the sandboxId, never the internal UID")
	require.Equal(t, "spi-signer", served.Credential)
	require.Equal(t, "internal-api", served.Binding)
	require.Equal(t, "api.internal.example.com:443", served.Authority)
	require.Equal(t, "/v1/foo", served.Path)

	var document struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &document))
	values := map[string]string{}
	for _, header := range document.Headers {
		values[header.Name] = header.Value
	}
	require.Equal(t, sandboxName, values["x-test-subject"])
}

func TestFastSandboxResolveSubjectUnknownIPs(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	fake := startFakeResolverServer(t, 0)
	s := bindNamedAndReady(t, srv, "uid-1", "fsb-a", testIP, testFenceR, testFenceA, 1)
	seedExchangeVault(t, srv, s, reg, fake.endpoint())

	handler := resolveHandlerFor(srv)
	cases := []struct {
		name    string
		target  string
		status  int
		message string
	}{
		{"unknown ip", "10.0.0.99", http.StatusNotFound, "no subject for clientIp"},
		{"missing clientIp", "", http.StatusBadRequest, "clientIp query parameter required"},
		{"malformed clientIp", "not-an-ip", http.StatusBadRequest, "invalid clientIp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := fastSandboxResolveRequest(tc.target)
			if tc.target == "" {
				request = httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve", strings.NewReader(
					`{"binding":"internal-api","sni":"api.internal.example.com",`+
						`"request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/"}}`))
				request.Header.Set("Content-Type", "application/json")
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			require.Equal(t, tc.status, recorder.Code)
			require.Contains(t, recorder.Body.String(), tc.message)
		})
	}
}

func TestFastSandboxResolveAttachmentIPMismatch(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	fake := startFakeResolverServer(t, 0)
	s := bindNamedAndReady(t, srv, "uid-1", "fsb-a", testIP, testFenceR, testFenceA, 1)
	seedExchangeVault(t, srv, s, reg, fake.endpoint())

	// A second subject registered at another IP: dispatch by IP must not
	// cross the two subjects' vaults.
	s2 := bindNamedAndReady(t, srv, "uid-2", "fsb-b", "10.0.0.9", "r-2", "a-2", 1)
	seedExchangeVault(t, srv, s2, reg, fake.endpoint())

	handler := resolveHandlerFor(srv)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, fastSandboxResolveRequest("10.0.0.9"))
	require.Equal(t, http.StatusOK, recorder.Code)
	served := <-fake.requests
	require.Equal(t, "fsb-b", served.Subject)
}

func TestFastSandboxResolveTakeoverInvalidatesInFlight(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	fake := startFakeResolverServer(t, 1500*time.Millisecond)

	s := bindNamedAndReady(t, srv, "uid-1", "fsb-original", testIP, testFenceR, testFenceA, 1)
	seedExchangeVault(t, srv, s, reg, fake.endpoint())

	handler := resolveHandlerFor(srv)
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, fastSandboxResolveRequest(testIP))
		completed <- recorder
	}()

	// Wait until the resolver call is in flight, then rebind the slot: a
	// successor SET_BINDING (new fence, new sandboxId) takes the subject over
	// while the old identity's resolution is still running.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(fake.requests) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	require.Len(t, fake.requests, 1)
	<-fake.requests
	takeover := doAction(t, srv, bindBodyWithName(t, "uid-1", "fsb-successor", testIP, "runtime-2", "attachment-2", 2, strPtr(resolveTestPolicy)))
	require.Equal(t, http.StatusOK, takeover.Code)

	select {
	case recorder := <-completed:
		require.Equal(t, http.StatusConflict, recorder.Code)
		require.Equal(t, "resolution invalidated", strings.TrimSpace(recorder.Body.String()))
	case <-time.After(10 * time.Second):
		t.Fatal("resolve did not complete")
	}
}

func TestFastSandboxResolveFailureLeavesPersistentStateUntouched(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	// A resolver endpoint that accepts nothing: every call is unavailable.
	deadDir, deadErr := os.MkdirTemp("", "osbr-dead-*")
	require.NoError(t, deadErr)
	t.Cleanup(func() { _ = os.RemoveAll(deadDir) })
	deadSocket := deadDir + "/dead.sock"
	listener, err := net.Listen("unix", deadSocket)
	require.NoError(t, err)
	dead := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.NotFoundHandler()}}
	dead.Start()
	t.Cleanup(dead.Close)

	s := bindNamedAndReady(t, srv, "uid-1", "fsb-x", testIP, testFenceR, testFenceA, 1)
	seedExchangeVault(t, srv, s, reg, "unix://"+deadSocket)

	handler := resolveHandlerFor(srv)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, fastSandboxResolveRequest(testIP))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	// Red line: resolve failures never touch degraded / unknown / the sticky
	// recovery latch — that is disk-indeterminate semantics only.
	srv.mu.Lock()
	entry, hasEntry := srv.vaults[s]
	rebind := srv.rebinds[s]
	srv.mu.Unlock()
	require.True(t, hasEntry)
	require.Empty(t, entry.degraded)
	require.False(t, entry.vaultStateUnknown)
	require.Nil(t, rebind)
	require.Nil(t, srv.vaultRecovery.Load(), "recovery latch must stay unset")
	health := httptest.NewRecorder()
	srv.handleHealthz(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, health.Code)

	// The vault itself keeps serving snapshots (state is not poisoned).
	recorder = httptest.NewRecorder()
	srv.handleCredentialVaultActive(recorder, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+testIP, nil))
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestFastSandboxResolveSemaphoreSaturationDoesNotBlockActive(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	t.Setenv(constants.EnvCredentialResolverMaxConcurrency, "2")
	fake := startFakeResolverServer(t, 1200*time.Millisecond)

	s := bindNamedAndReady(t, srv, "uid-1", "fsb-sat", testIP, testFenceR, testFenceA, 1)
	seedExchangeVault(t, srv, s, reg, fake.endpoint())

	handler := resolveHandlerFor(srv)
	var inFlight atomic.Int32
	for i := 0; i < 6; i++ {
		go func() {
			inFlight.Add(1)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, fastSandboxResolveRequest(testIP))
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && inFlight.Load() < 6 {
		time.Sleep(10 * time.Millisecond)
	}

	// While resolves saturate the semaphore, the non-exchange surface answers
	// promptly (no shared lock is held across the resolver call).
	started := time.Now()
	recorder := httptest.NewRecorder()
	srv.handleCredentialVaultActive(recorder, httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+testIP, nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Less(t, time.Since(started), 500*time.Millisecond, "active snapshot must not wait on the resolver semaphore")
}

func TestFastSandboxResolveWireSubjectRefreshesWithFence(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	fake := startFakeResolverServer(t, 0)

	bindNamedAndReady(t, srv, "uid-1", "fsb-first", testIP, testFenceR, testFenceA, 1)
	// Same subject, new fence: SET_BINDING replays with a new sandboxId and
	// the mirror must refresh (the old name never goes back on the wire).
	s := bindNamedAndReady(t, srv, "uid-1", "fsb-second", testIP, "runtime-2", "attachment-2", 2)
	seedExchangeVault(t, srv, s, reg, fake.endpoint())

	handler := resolveHandlerFor(srv)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, fastSandboxResolveRequest(testIP))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	served := <-fake.requests
	require.Equal(t, "fsb-second", served.Subject)
}

// ---------------------------------------------------------------------------
// Sidecar profile
// ---------------------------------------------------------------------------

func TestSidecarResolveVaultAccessEnvSubject(t *testing.T) {
	s := &policyServer{credentialVault: credentialvault.NewStore(nil, nil)}

	// Without the injected identity the endpoint fails closed before any
	// resolver interaction.
	t.Setenv(constants.EnvSandboxID, "")
	_, accessErr := s.resolveVaultAccess(nil)
	require.NotNil(t, accessErr)
	require.Equal(t, http.StatusServiceUnavailable, accessErr.Status)
	require.Equal(t, "sandbox identity not configured", accessErr.Reason)

	t.Setenv(constants.EnvSandboxID, " sbx-sidecar-7 ")
	access, accessErr := s.resolveVaultAccess(nil)
	require.Nil(t, accessErr)
	require.Equal(t, "sbx-sidecar-7", access.SandboxID)
	require.Same(t, s.credentialVault, access.Store)
}

func TestPolicyServerResolveSubresourceForbiddenOnTCP(t *testing.T) {
	s := &policyServer{}
	req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve", nil)
	rec := httptest.NewRecorder()
	s.handleCredentialVaultSubresource(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "forbidden", strings.TrimSpace(rec.Body.String()))

	req = httptest.NewRequest(http.MethodGet, "/credential-vault/_resolve", nil)
	rec = httptest.NewRecorder()
	s.handleCredentialVaultSubresource(rec, req)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	require.Equal(t, "POST", rec.Header().Get("Allow"))
}
