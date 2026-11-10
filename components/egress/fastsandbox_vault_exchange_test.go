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

// OSEP-0025 T7 fast-sandbox exchange integration: relay round-trips with an
// exchange source (the addon snapshot must carry the marker with explicit
// empty arrays — the T1 clone-defect pin), rehydrate/restart/export lifecycle
// with no resolver listener and no persisted material (R12), and subject
// isolation across IP takeover.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// deadResolverEndpoint names a unix socket that never listens: every vault
// write and reload below must succeed without touching it (the write path
// renders exchange markers only, and recovery runs factory validation only).
const deadResolverEndpoint = "unix:///tmp/osep0025-t7-no-listener.sock"

// exchangeVaultBody is the exchange-source vault shape the server relays
// (T5): an exchange credential referenced by a TYPELESS binding auth.
func exchangeVaultBody(endpoint string) string {
	return `{"credentials":[{"name":"spi-signer","source":{"type":"exchange","endpoint":"` + endpoint + `"}}],` +
		`"bindings":[{"name":"api","match":{"hosts":["api.example.com"],"methods":["GET"],"paths":["/v1/*"]},` +
		`"auth":{"credential":"spi-signer"}}]}`
}

// bindExchangeSubject drives a subject to active whose binding policy covers
// api.example.com and whose SandboxRef.Name is the wire sandboxId.
func bindExchangeSubject(t *testing.T, srv *fastSandboxPolicyServer, uid, name, ip, runtimeID, attID string, specGen uint64) subject.Subject {
	t.Helper()
	rec := doAction(t, srv, bindBodyWithName(t, uid, name, ip, runtimeID, attID, specGen, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, uid, runtimeID, attID, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, uid, runtimeID, attID, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	return subject.FromSandboxUID(uid)
}

// exchangeResolveRequest builds one addon resolve RPC for the api.example.com
// exchange binding of the subject dispatched at ip.
func exchangeResolveRequest(ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/credential-vault/_resolve?clientIp="+ip, strings.NewReader(
		`{"binding":"api","sni":"api.example.com",`+
			`"request":{"scheme":"https","host":"api.example.com","port":443,"method":"GET","path":"/v1/foo"}}`))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// activeSnapshotFor fetches the addon-facing active snapshot for a client IP.
func activeSnapshotFor(t *testing.T, srv *fastSandboxPolicyServer, ip string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+ip, nil)
	rec := httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec, req)
	return rec.Code, rec.Body.String()
}

// ---------------------------------------------------------------------------
// Relay round-trip with an exchange source (headers:[] pin)
// ---------------------------------------------------------------------------

// TestVaultRelayExchangeRoundTripPinsEmptyHeadersArray: a relay PUT/GET with
// an exchange source round-trips the endpoint (configuration, not material)
// and the addon snapshot renders the marker binding with EXPLICIT empty
// arrays — "headers":[] never "headers":null. A clone that degrades the
// empty array to null fails the addon parser closed (the T1 review fix).
// The endpoint has no listener: the write path performs zero network I/O.
func TestVaultRelayExchangeRoundTripPinsEmptyHeadersArray(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	s := vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPost, vaultTestUID, exchangeVaultBody(deadResolverEndpoint), "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"revision":1`)

	// GET returns the full revision: the exchange source (endpoint) and the
	// typeless auth survive the round-trip verbatim.
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	full := rec.Body.String()
	assert.Contains(t, full, `"revision":1`)
	assert.Contains(t, full, `"type":"exchange"`)
	assert.Contains(t, full, deadResolverEndpoint)
	assert.Contains(t, full, `"auth":{"credential":"spi-signer"}`)

	// The GET body is re-pushable as a replace body (the server-side PATCH
	// merge composes exactly this shape).
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, full, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"revision":2`)

	// The addon snapshot: marker binding, explicit empty arrays, no rendered
	// values, no redactions (nothing was ever resolved).
	code, snapshot := activeSnapshotFor(t, srv, vaultTestIP)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, snapshot, `"exchange":{"credential":"spi-signer"}`)
	assert.Contains(t, snapshot, `"headers":[]`)
	assert.Contains(t, snapshot, `"substitutions":[]`)
	assert.NotContains(t, snapshot, `"headers":null`)
	assert.NotContains(t, snapshot, `"substitutions":null`)
	assert.NotContains(t, snapshot, `"redactions"`)
	assert.NotContains(t, snapshot, "Bearer")

	// The conditional fetch answers 304 on the unchanged revision: the
	// snapshot shape is stable across reads, not just on first render.
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil)
	first := httptest.NewRecorder()
	srv.handleCredentialVaultActive(first, req)
	require.Equal(t, http.StatusOK, first.Code)
	etag := first.Header().Get("Etag")
	require.NotEmpty(t, etag)
	conditional := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil)
	conditional.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	srv.handleCredentialVaultActive(second, conditional)
	require.Equal(t, http.StatusNotModified, second.Code)

	_ = s
}

// ---------------------------------------------------------------------------
// IP reuse / takeover: wire-subject isolation
// ---------------------------------------------------------------------------

// TestVaultExchangeIPTakeoverIsolatesWireSubjects: the same slot IP handed
// from one subject to another (a different UID claiming the dispatch key)
// must never leak the predecessor's vault or wire identity: the old
// subject's in-flight resolution is invalidated (409), the successor starts
// with no vault, and once it installs its own, the wire subject is the
// successor's sandboxId on every resolve.
func TestVaultExchangeIPTakeoverIsolatesWireSubjects(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	slow := startFakeResolverServer(t, 1200*time.Millisecond)
	fast := startFakeResolverServer(t, 0)

	// Generation one: uid-a with sandboxId fsb-take-a and an exchange vault
	// bound to the slow resolver.
	sA := bindExchangeSubject(t, srv, "takeover-a", "fsb-take-a", testIP, testFenceR, testFenceA, 1)
	seedExchangeVaultFor(t, srv, sA, reg, "api", slow.endpoint())

	// In-flight resolution under the old identity.
	handler := resolveHandlerFor(srv)
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, exchangeResolveRequest(testIP))
		completed <- recorder
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(slow.requests) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	require.Len(t, slow.requests, 1, "the old subject's resolve must be in flight at the resolver")
	<-slow.requests

	// Takeover: a DIFFERENT sandbox UID claims the same slot IP. The
	// registry evicts the stale subject and its vault slice is purged.
	sB := bindExchangeSubject(t, srv, "takeover-b", "fsb-take-b", testIP, "runtime-2", "attachment-2", 2)

	// The old subject's in-flight resolution is invalidated — never served
	// under the successor's identity.
	select {
	case recorder := <-completed:
		require.Equal(t, http.StatusConflict, recorder.Code)
		require.Equal(t, "resolution invalidated", strings.TrimSpace(recorder.Body.String()))
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight resolve never completed")
	}

	// The successor starts with no vault: nothing of the predecessor's
	// surfaces (the exchange binding does not resolve).
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, exchangeResolveRequest(testIP))
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Body.String(), "binding not found")

	// The successor installs its OWN vault (fast resolver) and every resolve
	// carries the successor's wire subject — the predecessor's endpoint is
	// never dialed again.
	seedExchangeVaultFor(t, srv, sB, reg, "api", fast.endpoint())
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, exchangeResolveRequest(testIP))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	served := <-fast.requests
	require.Equal(t, "fsb-take-b", served.Subject, "the wire subject must be the successor's sandboxId")
	require.Equal(t, "api.example.com:443", served.Authority)

	// A repeated resolve stays on the successor's identity.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, exchangeResolveRequest(testIP))
	require.Equal(t, http.StatusOK, recorder.Code)
	served = <-fast.requests
	require.Equal(t, "fsb-take-b", served.Subject)

	// The predecessor's resolver saw exactly the one invalidated request.
	require.Len(t, slow.requests, 0, "the taken-over subject's resolver must not be dialed again")
}

// seedExchangeVaultFor installs an exchange binding through the live store
// (the relay round-trip variant lives above).
func seedExchangeVaultFor(t *testing.T, srv *fastSandboxPolicyServer, s subject.Subject, reg *subject.MemoryRegistry, binding, endpoint string) {
	t.Helper()
	vault := srv.vaultFor(s)
	require.NotNil(t, vault)
	_, err := vault.Create(credentialvault.CreateRequest{
		Credentials: []credentialvault.Credential{{
			Name:   "spi-signer",
			Source: json.RawMessage(`{"type":"exchange","endpoint":"` + endpoint + `"}`),
		}},
		Bindings: []credentialvault.Binding{{
			Name:  binding,
			Match: credentialvault.Match{Hosts: []string{"api.example.com"}, Methods: []string{"GET"}, Paths: []string{"/v1/*"}},
			Auth:  credentialvault.Auth{Credential: "spi-signer"},
		}},
	}, reg.EffectivePolicy(s))
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Shared-process isolation: one slow resolver must not block others
// ---------------------------------------------------------------------------

// TestVaultExchangeSlowResolveDoesNotBlockOtherSubjects: a subject whose
// resolver is slow neither blocks another subject's resolve nor the
// non-resolve vault surface — no shared lock is held across the resolver
// call (OSEP-0025 caveat 7). The assertions are RELATIVE (B completes while
// A is still parked at its resolver), not wall-clock budgets that flake on
// a loaded CI runner; the 1.5s/0s resolver split makes the ordering
// unambiguous.
func TestVaultExchangeSlowResolveDoesNotBlockOtherSubjects(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	slow := startFakeResolverServer(t, 1500*time.Millisecond)
	fast := startFakeResolverServer(t, 0)

	sA := bindExchangeSubject(t, srv, "iso-a", "fsb-iso-a", testIP, testFenceR, testFenceA, 1)
	seedExchangeVaultFor(t, srv, sA, reg, "api", slow.endpoint())
	sB := bindExchangeSubject(t, srv, "iso-b", "fsb-iso-b", "10.0.0.9", "runtime-2", "attachment-2", 1)
	seedExchangeVaultFor(t, srv, sB, reg, "api", fast.endpoint())

	handler := resolveHandlerFor(srv)
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, exchangeResolveRequest(testIP))
		completed <- recorder
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(slow.requests) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	require.Len(t, slow.requests, 1, "the slow subject's resolve must be parked at its resolver")
	<-slow.requests
	slowDone := func() bool {
		select {
		case <-completed:
			return true
		default:
			return false
		}
	}

	// Another subject's resolve runs to completion WHILE the first is still
	// parked — a shared lock held across the resolver call would serialize
	// the two behind the 1.5s delay.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, exchangeResolveRequest("10.0.0.9"))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.False(t, slowDone(), "the fast subject resolved only after the parked resolve returned — resolves were serialized")
	served := <-fast.requests
	require.Equal(t, "fsb-iso-b", served.Subject)

	// The non-resolve vault surface (what non-bound hosts and snapshot
	// fetches ride) answers while the slow resolve stays parked too.
	code, snapshot := activeSnapshotFor(t, srv, testIP)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, snapshot, `"exchange":{"credential":"spi-signer"}`)
	require.False(t, slowDone(), "the active snapshot answered only after the parked resolve returned")

	select {
	case recorder := <-completed:
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("the slow resolve never completed")
	}
}

// ---------------------------------------------------------------------------
// Persistence: reload with no resolver listener (R12)
// ---------------------------------------------------------------------------

// TestVaultPersistExchangeReloadNeedsNoResolver: an egress restart rehydrates
// a persisted exchange vault with NO resolver listener anywhere — recovery
// runs factory validation only, never the network. The reloaded slice
// serves the marker snapshot; the file keeps carrying only the endpoint.
func TestVaultPersistExchangeReloadNeedsNoResolver(t *testing.T) {
	h := newVaultPersistHarness(t)
	h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 4, exchangeVaultBody(deadResolverEndpoint))

	h.readySubject(t, vaultTestUID, vaultTestIP)

	// Reload succeeded offline: not degraded, exact file revision served.
	status := mustStatus(t, h.srv)
	assert.Empty(t, status.DegradedVaults)
	rec := doVault(t, h.srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":4`)
	assert.Contains(t, rec.Body.String(), deadResolverEndpoint)

	// The addon snapshot carries the marker with the explicit empty arrays —
	// nothing was resolved at recovery, so nothing could be cached.
	code, snapshot := activeSnapshotFor(t, h.srv, vaultTestIP)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, snapshot, `"exchange":{"credential":"spi-signer"}`)
	assert.Contains(t, snapshot, `"headers":[]`)
	assert.NotContains(t, snapshot, "Bearer")

	// R12: the persisted file still carries exactly the delivered body —
	// the endpoint (configuration) and no resolved material exists on disk.
	raw := string(h.fileFor(vaultTestUID).data)
	assert.Contains(t, raw, deadResolverEndpoint)
	assert.NotContains(t, raw, `"headers"`)
	assert.NotContains(t, raw, "Bearer")
	assert.NotContains(t, raw, "x-test")
	vf, body := decodeFile(t, h.fileFor(vaultTestUID))
	assert.Equal(t, int64(4), vf.Revision)
	require.Len(t, body.Credentials, 1)
	require.Len(t, body.Bindings, 1)
	assert.Empty(t, body.Bindings[0].Auth.Type, "the persisted binding auth stays typeless")
}

// ---------------------------------------------------------------------------
// Restart: the first resolve after the fence is re-established
// ---------------------------------------------------------------------------

// TestVaultPersistExchangeRestartFirstResolve: after an egress restart the
// SET_BINDING replay re-maps the subject and rebuilds the sandboxId mirror;
// the FIRST resolve on the reloaded vault succeeds with the reloaded
// revision and the current wire subject.
func TestVaultPersistExchangeRestartFirstResolve(t *testing.T) {
	h := newVaultPersistHarness(t)
	fake := startFakeResolverServer(t, 0)
	const wireName = "fsb-restart-1"
	h.seedVaultFile(vaultTestUID, testFenceR, testFenceA, 3, exchangeVaultBody(fake.endpoint()))

	// Generation one serves the seeded revision.
	bindExchangeSubject(t, h.srv, vaultTestUID, wireName, vaultTestIP, testFenceR, testFenceA, 1)

	// Egress restart: a fresh server generation over the same vault files.
	srv2, _, _ := fastSandboxTestServer(t)
	vp2, err := newVaultPersistence(h.dir)
	require.NoError(t, err)
	vp2.newFile = h.vp.newFile
	srv2.SetVaultFiles(vp2)

	// The Fastlet's instanceId replay: SET_BINDING (same runtime fence, the
	// wire sandboxId again) + hooks. subjSandbox rebuilds from the envelope.
	bindExchangeSubject(t, srv2, vaultTestUID, wireName, vaultTestIP, testFenceR, testFenceA, 2)

	rec := doVault(t, srv2, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":3`)

	srv2.mu.Lock()
	mirrored := srv2.subjSandbox[subject.FromSandboxUID(vaultTestUID)]
	srv2.mu.Unlock()
	require.Equal(t, wireName, mirrored, "the restart's SET_BINDING replay must rebuild the sandboxId mirror")

	// The first resolve after the restart resolves against the live
	// resolver with the rebuilt identity and the reloaded revision.
	handler := resolveHandlerFor(srv2)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, exchangeResolveRequest(vaultTestIP))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	served := <-fake.requests
	require.Equal(t, wireName, served.Subject)
	require.Equal(t, "api", served.Binding)
	require.Equal(t, "api.example.com:443", served.Authority)

	var document struct {
		Revision int64 `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &document))
	assert.Equal(t, int64(3), document.Revision, "the resolve answers from the reloaded revision")
}

// ---------------------------------------------------------------------------
// VAULT_EXPORT / VAULT_IMPORT with an exchange source (R12)
// ---------------------------------------------------------------------------

// TestVaultSnapshotExchangeExportImportCarriesNoMaterial: a pause/resume
// round-trip carries the exchange vault (endpoint + typeless binding) and
// NOTHING else: no resolved header values, no redaction values — resolution
// output never existed outside one request. While sealed the vault fails
// closed; after the resume-side import + replay the vault resolves again
// under the restored fence with the new wire sandboxId.
func TestVaultSnapshotExchangeExportImportCarriesNoMaterial(t *testing.T) {
	h := newVaultPersistHarness(t)
	fake := startFakeResolverServer(t, 0)
	const pausedName = "fsb-snap-a"
	s := bindExchangeSubject(t, h.srv, snapshotTestUID, pausedName, snapshotTestIP, testFenceR, testFenceA, 1)
	rec := doVault(t, h.srv, http.MethodPost, snapshotTestUID, exchangeVaultBody(fake.endpoint()), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// Material flows once (per-request only), then the pause exports.
	handler := resolveHandlerFor(h.srv)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, exchangeResolveRequest(snapshotTestIP))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"name":"x-test-subject"`)
	<-fake.requests

	bundlePath := filepath.Join(t.TempDir(), "bundle", snapshotTestUID+".json")
	rec = doAction(t, h.srv, snapshotBody(t, snapshotTestUID, testFenceR, testFenceA, "VAULT_EXPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)

	// The bundle is the vault file verbatim: v1, the owning fence, the
	// revision, and a body of exactly one exchange credential + one
	// typeless binding.
	bundle := h.fakeForPath(bundlePath)
	require.True(t, bundle.exists)
	raw := string(bundle.data)
	assert.Contains(t, raw, fake.endpoint())
	assert.Contains(t, raw, `"auth":{"credential":"spi-signer"}`)
	// R12: no resolution material anywhere in the bundle.
	assert.NotContains(t, raw, `"headers"`)
	assert.NotContains(t, raw, `"redactions"`)
	assert.NotContains(t, raw, "Bearer")
	assert.NotContains(t, raw, "x-test-subject")
	assert.NotContains(t, raw, "x-test-signature")

	// Sealed: the vault fails closed for resolve (the bundle is taken).
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, exchangeResolveRequest(snapshotTestIP))
	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), "sealed")

	// Stage the exported bytes for the resume side (import reads the
	// driver-staged source with plain I/O — not the file-store seam).
	require.NoError(t, os.MkdirAll(filepath.Dir(bundlePath), 0o700))
	require.NoError(t, os.WriteFile(bundlePath, []byte(raw), 0o600))

	// Resume on a fresh egress generation: import before SET_BINDING, then
	// the replay under the restored fence with a NEW wire sandboxId.
	srv2, _, _ := fastSandboxTestServer(t)
	vp2, err := newVaultPersistence(h.dir)
	require.NoError(t, err)
	vp2.newFile = h.vp.newFile
	srv2.SetVaultFiles(vp2)
	const resumedName = "fsb-snap-b"
	rec = doAction(t, srv2, snapshotBody(t, snapshotTestUID, snapshotResumeR, snapshotResumeA, "VAULT_IMPORT", bundlePath))
	require.Equal(t, http.StatusOK, rec.Code)
	bindExchangeSubject(t, srv2, snapshotTestUID, resumedName, snapshotTestIP, snapshotResumeR, snapshotResumeA, 1)

	// The re-stamped vault resolves under the resumed identity.
	handler2 := resolveHandlerFor(srv2)
	recorder = httptest.NewRecorder()
	handler2.ServeHTTP(recorder, exchangeResolveRequest(snapshotTestIP))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	served := <-fake.requests
	require.Equal(t, resumedName, served.Subject, "the resumed vault resolves under the restored fence's wire subject")

	_ = s
}
