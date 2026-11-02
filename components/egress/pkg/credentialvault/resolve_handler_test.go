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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// fakeResolveClient is an in-process ResolverClient: records inputs, blocks
// each call until release, answers with a fixed decision or error.
type fakeResolveClient struct {
	mu       sync.Mutex
	inputs   []ResolveInput
	decision *ResolverDecision
	err      error
	release  chan struct{}
}

func newFakeResolveClient() *fakeResolveClient {
	return &fakeResolveClient{release: make(chan struct{})}
}

func (f *fakeResolveClient) Resolve(_ context.Context, input ResolveInput) (*ResolverDecision, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, input)
	f.mu.Unlock()
	<-f.release
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.decision, f.err
}

func (f *fakeResolveClient) recorded() []ResolveInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ResolveInput(nil), f.inputs...)
}

func (f *fakeResolveClient) unblock() { close(f.release) }

// resolveHarness wires a handler around one store and one fake client.
type resolveHarness struct {
	store    *Store
	client   *fakeResolveClient
	handler  *ResolveHandler
	access   ResolveAccess
	audits   []ResolveAuditEvent
	newCalls int
}

func newResolveHarness(t *testing.T) *resolveHarness {
	t.Helper()
	store := testResolverReadyStore()
	_, createErr := store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.NoError(t, createErr)
	harness := &resolveHarness{
		store:   store,
		client:  newFakeResolveClient(),
		access:  ResolveAccess{SandboxID: "sbx_01HZYEXAMPLE", Store: store},
		audits:  nil,
		handler: nil,
	}
	harness.handler = NewResolveHandler(func(*http.Request) (ResolveAccess, *ResolveAccessError) {
		return harness.access, nil
	})
	harness.handler.newClient = func(string) (ResolverClient, error) {
		harness.newCalls++
		return harness.client, nil
	}
	harness.handler.audit = func(event ResolveAuditEvent) { harness.audits = append(harness.audits, event) }
	return harness
}

// postResolve issues one resolve RPC against the harness handler.
func (h *resolveHarness) postResolve(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func validResolveBody() string {
	return `{"binding":"internal-api","sni":"api.internal.example.com",` +
		`"request":{"scheme":"https","host":"api.internal.example.com","port":443,"method":"GET","path":"/v1/foo"}}`
}

func TestResolveHandlerHappyPath(t *testing.T) {
	h := newResolveHarness(t)
	h.client.decision = &ResolverDecision{
		Headers:  map[string]string{"authorization": "Bearer secret-value", "x-signature": "sig"},
		Suppress: map[string]struct{}{"authorization": {}, "x-signature": {}},
		Redactions: []string{
			"Bearer secret-value",
			"secret-value",
			"sig",
		},
	}

	h.client.unblock()
	rec := h.postResolve(validResolveBody())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var document resolveEndpointResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &document))
	require.Equal(t, int64(1), document.Revision)
	require.Equal(t, "internal-api", document.Binding)
	require.Equal(t, []resolveResponseHeader{
		{Name: "authorization", Value: "Bearer secret-value"},
		{Name: "x-signature", Value: "sig"},
	}, document.Headers)
	require.Equal(t, []string{"authorization", "x-signature"}, document.SuppressHeaders)
	require.Equal(t, []string{"Bearer secret-value", "secret-value", "sig"}, document.Redactions)

	// Wire subject is the resolved sandboxId; authority carries the default
	// port; frame headers serialize as an empty object.
	recorded := h.client.recorded()
	require.Len(t, recorded, 1)
	require.Equal(t, "sbx_01HZYEXAMPLE", recorded[0].Subject)
	require.Equal(t, "spi-signer", recorded[0].Credential)
	require.Equal(t, "internal-api", recorded[0].Binding)
	require.Equal(t, ResolveDestination{
		Scheme:    "https",
		Authority: "api.internal.example.com:443",
		SNI:       "api.internal.example.com",
	}, recorded[0].Destination)
	require.Equal(t, ResolveRequestFrame{
		Method:  "GET",
		Host:    "api.internal.example.com",
		Path:    "/v1/foo",
		Headers: map[string]string{},
	}, recorded[0].Request)

	require.Equal(t, 1, h.newCalls)
	require.Len(t, h.audits, 1)
	require.Equal(t, "resolved", h.audits[0].Resolution)
	require.Equal(t, "spi-signer", h.audits[0].Credential)
	require.Equal(t, "internal-api", h.audits[0].Binding)
	require.Equal(t, "api.internal.example.com", h.audits[0].TargetHost)
	require.Equal(t, "sbx_01HZYEXAMPLE", h.audits[0].Subject)
}

func TestResolveHandlerPortAuthorityNormalization(t *testing.T) {
	h := newResolveHarness(t)
	h.client.decision = &ResolverDecision{Headers: map[string]string{}, Suppress: map[string]struct{}{}, Redactions: []string{}}
	h.client.unblock()

	body := `{"binding":"internal-api","sni":"api.internal.example.com",` +
		`"request":{"scheme":"http","host":"api.internal.example.com","method":"get","path":"/x"}}`
	rec := h.postResolve(body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "api.internal.example.com:80", h.client.recorded()[0].Destination.Authority)
	require.Equal(t, "GET", h.client.recorded()[0].Request.Method)

	body = `{"binding":"internal-api","sni":"api.internal.example.com",` +
		`"request":{"scheme":"https","host":"API.internal.example.com.","port":8443,"method":"GET","path":"/x"}}`
	rec = h.postResolve(body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "api.internal.example.com:8443", h.client.recorded()[1].Destination.Authority)
	require.Equal(t, "api.internal.example.com", h.client.recorded()[1].Destination.SNI)
}

func TestResolveHandlerRequestDiscipline(t *testing.T) {
	h := newResolveHarness(t)
	cases := []struct {
		name    string
		method  string
		typeCT  string
		body    string
		status  int
		message string
	}{
		{"wrong method", http.MethodGet, "application/json", validResolveBody(), http.StatusMethodNotAllowed, "method not allowed"},
		{"wrong content type", http.MethodPost, "text/plain", validResolveBody(), http.StatusBadRequest, "malformed request"},
		{"charset content type", http.MethodPost, "application/json; charset=utf-8", validResolveBody(), http.StatusBadRequest, "malformed request"},
		{"missing content type", http.MethodPost, "", validResolveBody(), http.StatusBadRequest, "malformed request"},
		{"not json", http.MethodPost, "application/json", `nope`, http.StatusBadRequest, "malformed request"},
		{"duplicate key", http.MethodPost, "application/json",
			`{"binding":"a","binding":"a","sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"duplicate request key", http.MethodPost, "application/json",
			`{"binding":"a","sni":"api.internal.example.com","request":{"scheme":"https","scheme":"http","host":"api.internal.example.com","method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"unknown field", http.MethodPost, "application/json",
			`{"binding":"a","sni":"api.internal.example.com","extra":1,"request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"unknown request field", http.MethodPost, "application/json",
			`{"binding":"a","sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/","headers":{}}}`,
			http.StatusBadRequest, "malformed request"},
		{"missing request", http.MethodPost, "application/json", `{"binding":"a","sni":"api.internal.example.com"}`, http.StatusBadRequest, "malformed request"},
		{"null binding", http.MethodPost, "application/json",
			`{"binding":null,"sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"blank binding", http.MethodPost, "application/json",
			`{"binding":"  ","sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"bad scheme", http.MethodPost, "application/json",
			`{"binding":"internal-api","sni":"api.internal.example.com","request":{"scheme":"ftp","host":"api.internal.example.com","method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"float port", http.MethodPost, "application/json",
			`{"binding":"internal-api","sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","port":443.5,"method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"null port", http.MethodPost, "application/json",
			`{"binding":"internal-api","sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","port":null,"method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"out-of-range port", http.MethodPost, "application/json",
			`{"binding":"internal-api","sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","port":65536,"method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"ip host", http.MethodPost, "application/json",
			`{"binding":"internal-api","sni":"10.0.0.5","request":{"scheme":"https","host":"10.0.0.5","method":"GET","path":"/"}}`,
			http.StatusBadRequest, "malformed request"},
		{"path without slash", http.MethodPost, "application/json",
			`{"binding":"internal-api","sni":"api.internal.example.com","request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"v1"}}`,
			http.StatusBadRequest, "malformed request"},
		{"trailing data", http.MethodPost, "application/json", validResolveBody() + " {}", http.StatusBadRequest, "malformed request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/credential-vault/_resolve", strings.NewReader(tc.body))
			if tc.typeCT != "" {
				req.Header.Set("Content-Type", tc.typeCT)
			}
			rec := httptest.NewRecorder()
			h.handler.ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, tc.message, strings.TrimSpace(rec.Body.String()))
		})
	}

	// Over-limit bodies stay within the closed status table (400).
	req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve",
		strings.NewReader(`{"binding":"`+strings.Repeat("x", maxResolveRequestBytes)+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "request too large", strings.TrimSpace(rec.Body.String()))

	// Nothing reached the resolver.
	require.Empty(t, h.client.recorded())
}

func TestResolveHandlerSNIGate(t *testing.T) {
	h := newResolveHarness(t)
	h.client.decision = &ResolverDecision{Headers: map[string]string{}, Suppress: map[string]struct{}{}, Redactions: []string{}}
	h.client.unblock()

	body := `{"binding":"internal-api","sni":"other.internal.example.com",` +
		`"request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/v1/foo"}}`
	rec := h.postResolve(body)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "forbidden", strings.TrimSpace(rec.Body.String()))
	require.Empty(t, h.client.recorded())

	// Case and trailing-dot differences normalize to equality.
	body = `{"binding":"internal-api","sni":"API.internal.example.com.",` +
		`"request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/v1/foo"}}`
	rec = h.postResolve(body)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestResolveHandlerUnknownSubjectsAndBindings(t *testing.T) {
	h := newResolveHarness(t)
	h.client.unblock()

	rec := h.postResolve(`{"binding":"missing","sni":"api.internal.example.com",` +
		`"request":{"scheme":"https","host":"api.internal.example.com","method":"GET","path":"/"}}`)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, "binding not found", strings.TrimSpace(rec.Body.String()))

	// A static (non-exchange) binding is not resolvable through the RPC.
	staticStore := testResolverReadyStore()
	_, err := staticStore.Create(CreateRequest{
		Credentials: []Credential{{
			Name:   "inline-cred",
			Source: mustMarshal(map[string]string{"type": "inline", "value": "v"}),
		}},
		Bindings: []Binding{{
			Name:  "static-api",
			Match: Match{Hosts: []string{"api.internal.example.com"}, Methods: []string{"GET"}, Paths: []string{"/*"}},
			Auth:  Auth{Type: "bearer", Credential: "inline-cred"},
		}},
	}, testExchangePolicy(t))
	require.NoError(t, err)
	hStatic := newResolveHarness(t)
	hStatic.access = ResolveAccess{SandboxID: "sbx-static", Store: staticStore}
	hStatic.client.decision = &ResolverDecision{Headers: map[string]string{}, Suppress: map[string]struct{}{}, Redactions: []string{}}
	hStatic.client.unblock()
	rec = hStatic.postResolve(validResolveBody())
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Access failures propagate their own fixed status.
	h.handler = NewResolveHandler(func(*http.Request) (ResolveAccess, *ResolveAccessError) {
		return ResolveAccess{}, &ResolveAccessError{Status: http.StatusServiceUnavailable, Reason: "sandbox identity unavailable"}
	})
	rec = h.postResolve(validResolveBody())
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "sandbox identity unavailable", strings.TrimSpace(rec.Body.String()))

	// A vault that does not exist is a binding-level 404.
	empty := testResolverReadyStore()
	h2 := newResolveHarness(t)
	h2.access = ResolveAccess{SandboxID: "sbx-x", Store: empty}
	h2.client.unblock()
	rec = h2.postResolve(validResolveBody())
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// startDelayedResolve launches one resolve RPC whose resolver call blocks
// until the returned release func, then returns the recorded input snapshot.
func startDelayedResolve(t *testing.T, h *resolveHarness) (release func(), recorded func() []ResolveInput, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	h.client.decision = &ResolverDecision{Headers: map[string]string{}, Suppress: map[string]struct{}{}, Redactions: []string{}}
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() { completed <- h.postResolve(validResolveBody()) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.client.recorded()) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	require.Len(t, h.client.recorded(), 1, "resolver call must start before mutations")
	return h.client.unblock, func() []ResolveInput { return h.client.recorded() }, completed
}

func TestResolveHandlerScopedInvalidationUnrelatedMutationSucceeds(t *testing.T) {
	h := newResolveHarness(t)
	release, _, done := startDelayedResolve(t, h)

	// Revision advances (new unrelated credential+binding) while the captured
	// binding is untouched: the in-flight resolution must still succeed.
	_, err := h.store.Patch(MutationRequest{
		Credentials: &CredentialMutationSet{Add: []Credential{{
			Name:   "inline-2",
			Source: mustMarshal(map[string]string{"type": "inline", "value": "v2"}),
		}}},
		Bindings: &BindingMutationSet{Add: []Binding{{
			Name:  "other-api",
			Match: Match{Hosts: []string{"other.internal.example.com"}, Methods: []string{"GET"}, Paths: []string{"/*"}},
			Auth:  Auth{Type: "bearer", Credential: "inline-2"},
		}}},
	}, testCredentialPolicy(t, `{"defaultAction":"deny","egress":[
		{"action":"allow","target":"api.internal.example.com"},
		{"action":"allow","target":"other.internal.example.com"}]}`))
	require.NoError(t, err)

	release()
	select {
	case rec := <-done:
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `"revision":1`, "captured revision is reported")
	case <-time.After(5 * time.Second):
		t.Fatal("resolve did not complete")
	}
}

func TestResolveHandlerScopedInvalidationBindingTouchedFails(t *testing.T) {
	original := testExchangeVaultRequest()
	cases := []struct {
		name string
		// mutate runs while the resolve call is in flight.
		mutate func(t *testing.T, store *Store)
	}{
		{"binding replaced", func(t *testing.T, store *Store) {
			replaced := original.Bindings[0]
			replaced.Match.Paths = []string{"/v2/*"}
			_, err := store.Patch(MutationRequest{Bindings: &BindingMutationSet{Replace: []Binding{replaced}}}, testExchangePolicy(t))
			require.NoError(t, err)
		}},
		{"binding replaced same revision content identical", func(t *testing.T, store *Store) {
			// A content-identical replacement does not invalidate.
			replaced := original.Bindings[0]
			_, err := store.Patch(MutationRequest{Bindings: &BindingMutationSet{Replace: []Binding{replaced}}}, testExchangePolicy(t))
			require.NoError(t, err)
		}},
		{"binding deleted then recreated different content", func(t *testing.T, store *Store) {
			_, err := store.Patch(MutationRequest{Bindings: &BindingMutationSet{Delete: []string{"internal-api"}}}, testExchangePolicy(t))
			require.NoError(t, err)
			recreated := original.Bindings[0]
			recreated.Match.Methods = []string{"GET", "POST"}
			_, err = store.Patch(MutationRequest{Bindings: &BindingMutationSet{Add: []Binding{recreated}}}, testExchangePolicy(t))
			require.NoError(t, err)
		}},
		{"credential replaced", func(t *testing.T, store *Store) {
			_, err := store.Patch(MutationRequest{Credentials: &CredentialMutationSet{Replace: []Credential{{
				Name:   "spi-signer",
				Source: mustMarshal(map[string]string{"type": "exchange", "endpoint": "unix:///run/spi/other.sock"}),
			}}}}, testExchangePolicy(t))
			require.NoError(t, err)
		}},
		{"vault deleted", func(t *testing.T, store *Store) {
			require.NoError(t, store.Delete())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResolveHarness(t)
			release, _, done := startDelayedResolve(t, h)
			tc.mutate(t, h.store)
			release()
			select {
			case rec := <-done:
				switch tc.name {
				case "binding replaced same revision content identical":
					require.Equal(t, http.StatusOK, rec.Code, "identical content is not a replacement")
				default:
					require.Equal(t, http.StatusConflict, rec.Code)
					require.Equal(t, "resolution invalidated", strings.TrimSpace(rec.Body.String()))
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resolve did not complete")
			}
		})
	}
}

func TestResolveHandlerSubjectReplacementInvalidates(t *testing.T) {
	storeA := testResolverReadyStore()
	_, createErrA := storeA.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.NoError(t, createErrA)
	storeB := testResolverReadyStore()
	_, createErrB := storeB.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.NoError(t, createErrB)

	mu := sync.Mutex{}
	current := ResolveAccess{SandboxID: "sbx-original", Store: storeA}
	handler := NewResolveHandler(func(*http.Request) (ResolveAccess, *ResolveAccessError) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	})
	client := newFakeResolveClient()
	handler.newClient = func(string) (ResolverClient, error) { return client, nil }
	client.decision = &ResolverDecision{Headers: map[string]string{}, Redactions: []string{}}

	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve", strings.NewReader(validResolveBody()))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		completed <- rec
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(client.recorded()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	require.Len(t, client.recorded(), 1)

	// Takeover: the identity re-derivation now yields a different sandbox /
	// store — the resolved headers must be discarded.
	mu.Lock()
	current = ResolveAccess{SandboxID: "sbx-successor", Store: storeB}
	mu.Unlock()
	client.unblock()

	select {
	case rec := <-completed:
		require.Equal(t, http.StatusConflict, rec.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("resolve did not complete")
	}
}

func TestResolveHandlerFailureMapping(t *testing.T) {
	cases := []struct {
		kind    ResolverErrorKind
		status  int
		message string
		outcome string
	}{
		{ResolverTimeout, http.StatusGatewayTimeout, "resolver timeout", "timeout"},
		{ResolverDenied, http.StatusForbidden, "forbidden", "denied"},
		{ResolverInvalidResponse, http.StatusBadRequest, "invalid resolver response", "denied"},
		{ResolverRejected, http.StatusServiceUnavailable, "resolver unavailable", "denied"},
		{ResolverUnavailable, http.StatusServiceUnavailable, "resolver unavailable", "denied"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			h := newResolveHarness(t)
			h.client.err = &ResolverError{Kind: tc.kind}
			h.client.unblock()
			rec := h.postResolve(validResolveBody())
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, tc.message, strings.TrimSpace(rec.Body.String()))
			require.Len(t, h.audits, 1)
			require.Equal(t, tc.outcome, h.audits[0].Resolution)
		})
	}

	// A non-typed error is an operational 503.
	h := newResolveHarness(t)
	h.client.err = errors.New("boom")
	h.client.unblock()
	rec := h.postResolve(validResolveBody())
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestResolveHandlerAuditNeverCarriesValues(t *testing.T) {
	secret := "super-secret-bearer-value"
	h := newResolveHarness(t)
	h.client.decision = &ResolverDecision{
		Headers:    map[string]string{"authorization": "Bearer " + secret},
		Suppress:   map[string]struct{}{"authorization": {}},
		Redactions: []string{secret},
	}
	h.client.unblock()

	rec := h.postResolve(validResolveBody())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, h.audits, 1)
	event := h.audits[0]
	require.Equal(t, "resolved", event.Resolution)
	encoded, err := json.Marshal(event)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)
	require.NotContains(t, string(encoded), "Bearer")

	// Denied outcomes carry no values either.
	h2 := newResolveHarness(t)
	h2.client.err = &ResolverError{Kind: ResolverTimeout}
	h2.client.unblock()
	rec = h2.postResolve(validResolveBody())
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	encoded, _ = json.Marshal(h2.audits[0])
	require.NotContains(t, string(encoded), "Bearer")
}

func TestResolveHandlerClientCachedPerEndpoint(t *testing.T) {
	h := newResolveHarness(t)
	h.client.decision = &ResolverDecision{Headers: map[string]string{}, Suppress: map[string]struct{}{}, Redactions: []string{}}
	h.client.unblock()
	require.Equal(t, http.StatusOK, h.postResolve(validResolveBody()).Code)
	require.Equal(t, http.StatusOK, h.postResolve(validResolveBody()).Code)
	require.Equal(t, 1, h.newCalls, "one endpoint must produce one client")
	require.Len(t, h.client.recorded(), 2)
}

func TestResolveHandlerClientCacheBounded(t *testing.T) {
	// The cache is never evicted; past the cap, clients are rebuilt per
	// request instead of growing the map without bound.
	h := newResolveHarness(t)
	h.client.decision = &ResolverDecision{Headers: map[string]string{}, Suppress: map[string]struct{}{}, Redactions: []string{}}
	h.client.unblock()
	// Rotate the captured endpoint past the cap by swapping the credential.
	for i := 0; i <= maxCachedResolverClients; i++ {
		_, err := h.store.Patch(MutationRequest{Credentials: &CredentialMutationSet{Replace: []Credential{{
			Name:   "spi-signer",
			Source: mustMarshal(map[string]string{"type": "exchange", "endpoint": fmt.Sprintf("unix:///run/spi/%d.sock", i)}),
		}}}}, testExchangePolicy(t))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, h.postResolve(validResolveBody()).Code)
	}
	require.Len(t, h.handler.clients, maxCachedResolverClients, "cache must not exceed the cap")
	require.Equal(t, maxCachedResolverClients+1, h.newCalls)
	// An endpoint still inside the cache keeps reusing its client.
	_, err := h.store.Patch(MutationRequest{Credentials: &CredentialMutationSet{Replace: []Credential{{
		Name:   "spi-signer",
		Source: mustMarshal(map[string]string{"type": "exchange", "endpoint": "unix:///run/spi/0.sock"}),
	}}}}, testExchangePolicy(t))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, h.postResolve(validResolveBody()).Code)
	require.Equal(t, maxCachedResolverClients+1, h.newCalls, "cached endpoint must not rebuild")
}

func TestResolverConfigFromEnvWiring(t *testing.T) {
	t.Setenv(constants.EnvCredentialResolverTimeout, "5")
	t.Setenv(constants.EnvCredentialResolverAuth, "https://resolver.internal:8443=token-1")
	config, diag := ResolverConfigFromEnv()
	require.NoError(t, diag)
	require.True(t, config.Ready())

	t.Setenv(constants.EnvCredentialResolverTimeout, "nope")
	config, diag = ResolverConfigFromEnv()
	require.Error(t, diag)
	require.False(t, config.TimeoutConfigured)
	require.True(t, config.AuthTableParsed)

	t.Setenv(constants.EnvCredentialResolverTimeout, "5")
	t.Setenv(constants.EnvCredentialResolverAuth, "https://resolver.internal:8443")
	config, diag = ResolverConfigFromEnv()
	require.Error(t, diag)
	require.False(t, config.AuthTableParsed)
	require.True(t, config.TimeoutConfigured)

	// Absent values keep defaults and count as ready.
	t.Setenv(constants.EnvCredentialResolverTimeout, "")
	t.Setenv(constants.EnvCredentialResolverAuth, "")
	config, diag = ResolverConfigFromEnv()
	require.NoError(t, diag)
	require.True(t, config.Ready())
}

func TestNewSubjectStoreAdmitsExchangeOnlyWithReadyEnv(t *testing.T) {
	t.Setenv(constants.EnvCredentialResolverAuth, "https://r=,broken") // malformed table
	store := NewSubjectStore()
	_, err := store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.ErrorContains(t, err, "exchange credentials are not supported until the credential resolver operator configuration is ready")

	t.Setenv(constants.EnvCredentialResolverAuth, "")
	store = NewSubjectStore()
	_, err = store.Create(testExchangeVaultRequest(), testExchangePolicy(t))
	require.NoError(t, err)
}

// Stub-backed end-to-end (tests/resolver/resolver.py): the wire subject, the
// deterministic signature, and timeout classification.

func TestResolveHandlerStubEndToEnd(t *testing.T) {
	socketPath := fmt.Sprintf("%s/r.sock", shortTempDir(t))
	stub := startStubResolver(t, nil, "--uds", socketPath)

	store := testResolverReadyStore()
	request := testExchangeVaultRequest()
	request.Credentials = []Credential{{
		Name:   "spi-signer",
		Source: mustMarshal(map[string]string{"type": "exchange", "endpoint": strings.Replace(stub.endpoint, "uds://", "unix://", 1)}),
	}}
	_, createErr := store.Create(request, testExchangePolicy(t))
	require.NoError(t, createErr)

	handler := NewResolveHandler(func(*http.Request) (ResolveAccess, *ResolveAccessError) {
		return ResolveAccess{SandboxID: "sbx_01HZYEXAMPLE", Store: store}, nil
	})
	body := validResolveBody()
	req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var document resolveEndpointResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &document))
	headers := map[string]string{}
	for _, header := range document.Headers {
		headers[header.Name] = header.Value
	}
	expected := stubSignature("sbx_01HZYEXAMPLE", "spi-signer", "internal-api", "api.internal.example.com:443", "/v1/foo")
	require.Equal(t, expected, headers["x-test-signature"])
	require.Equal(t, "sbx_01HZYEXAMPLE", headers["x-test-subject"], "wire subject must be the sandboxId (D11)")
	require.Contains(t, headers, "authorization")
	require.Contains(t, document.Redactions, headers["authorization"])
	require.Contains(t, document.SuppressHeaders, "authorization")
}

func TestResolveHandlerStubTimeout(t *testing.T) {
	t.Setenv(constants.EnvCredentialResolverTimeout, "1")
	socketPath := fmt.Sprintf("%s/r.sock", shortTempDir(t))
	stub := startStubResolver(t, nil, "--uds", socketPath, "--delay", "2000")

	store := testResolverReadyStore()
	request := testExchangeVaultRequest()
	request.Credentials = []Credential{{
		Name:   "spi-signer",
		Source: mustMarshal(map[string]string{"type": "exchange", "endpoint": strings.Replace(stub.endpoint, "uds://", "unix://", 1)}),
	}}
	_, createErr := store.Create(request, testExchangePolicy(t))
	require.NoError(t, createErr)
	handler := NewResolveHandler(func(*http.Request) (ResolveAccess, *ResolveAccessError) {
		return ResolveAccess{SandboxID: "sbx_01HZYEXAMPLE", Store: store}, nil
	})

	started := time.Now()
	rec := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve", strings.NewReader(validResolveBody()))
		req.Header.Set("Content-Type", "application/json")
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, req)
		return r
	}()
	require.Equal(t, http.StatusGatewayTimeout, rec.Code)
	require.Less(t, time.Since(started), 2*time.Second)
}
