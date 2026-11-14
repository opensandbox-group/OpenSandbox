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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
)

// ---------------------------------------------------------------------------
// Relay vault surface (OSEP-0022): /vault/{sandboxUid}
// ---------------------------------------------------------------------------

const (
	vaultTestUID = "vault-u"
	vaultTestIP  = "10.9.0.5"
)

// vaultAllowPolicy is the minimal effective policy a credential binding can
// validate against: the binding host must be explicitly allowed.
func vaultAllowPolicy() *string {
	input := `{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"}]}`
	return &input
}

func vaultReadySubject(t *testing.T, srv *fastSandboxPolicyServer, reg *subject.MemoryRegistry) subject.Subject {
	t.Helper()
	return setBindingAndReady(t, srv, reg, vaultTestUID, vaultTestIP, vaultAllowPolicy())
}

func vaultCredential() string {
	return `{"credentials":[{"name":"tok","source":{"type":"inline","value":"sekret"}}],` +
		`"bindings":[{"name":"api","match":{"hosts":["api.example.com"]},` +
		`"auth":{"type":"bearer","credential":"tok"}}]}`
}

func vaultFullBody() string {
	return `{"credentials":[{"name":"tok","source":{"type":"inline","value":"sekret"}}],` +
		`"bindings":[{"name":"api","match":{"hosts":["api.example.com"]},` +
		`"auth":{"type":"bearer","credential":"tok"}}]}`
}

// vaultPut is a conditional replace body pinning expectedRevision.
func vaultPut(revision string) string {
	return strings.TrimSuffix(vaultFullBody(), "}") + `,"expectedRevision":` + revision + `}`
}

// doVault drives one relay request against the surface; incarnation is the
// optional expectedIncarnation precondition header.
func doVault(t *testing.T, srv *fastSandboxPolicyServer, method, uid, body, incarnation string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, constants.VaultRelayPathPrefix+uid, strings.NewReader(body))
	if incarnation != "" {
		req.Header.Set(constants.EgressVaultIncarnationHeader, incarnation)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestVaultRelayUnknownSubject404NoCache(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := doVault(t, srv, method, "ghost", vaultCredential(), "")
		assert.Equal(t, http.StatusNotFound, rec.Code, method)
	}
	// No vault store was materialized for the unknown subject.
	srv.mu.Lock()
	_, stored := srv.vaults[subject.FromSandboxUID("ghost")]
	srv.mu.Unlock()
	assert.False(t, stored, "the relay surface never materializes state for an unregistered subject")
}

func TestVaultRelayPostCreateOnly(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)

	// Duplicate POST is a 409 — atomic at egress, safe across server replicas.
	rec = doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusConflict, rec.Code)

	// POST after a later replace is still a 409 — create-only, no overwrite.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":2`)

	rec = doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusConflict, rec.Code)
}

func TestVaultRelayPutCreatesAndPreconditions(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	vaultReadySubject(t, srv, reg)

	// PUT with no vault present creates (sidecar parity: expectedRevision optional).
	rec := doVault(t, srv, http.MethodPut, vaultTestUID, vaultFullBody(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)

	// Matching expectedRevision replaces.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":2`)

	// Stale expectedRevision is a 412 (sidecar PreconditionFailed parity).
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// DELETE, then a conditional PUT against the missing vault: 412, not create.
	rec = doVault(t, srv, http.MethodDelete, vaultTestUID, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("2"), "")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// Unconditional PUT recreates from scratch at revision 1.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultFullBody(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)
}

func TestVaultRelayCoverageFailureKeepsLastGood(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// A binding host the policy does not allow fails coverage: 400 NACK.
	bad := `{"credentials":[{"name":"tok","source":{"type":"inline","value":"sekret"}}],` +
		`"bindings":[{"name":"other","match":{"hosts":["other.example.com"]},` +
		`"auth":{"type":"bearer","credential":"tok"}}]}`
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, bad, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Last-good kept: the full revision is unchanged at revision 1.
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)
	assert.Contains(t, rec.Body.String(), `"api"`)
	assert.NotContains(t, rec.Body.String(), `"other"`)

	// Unparseable body is a 400 too.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, "{not json", "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestVaultRelayGetFullRevisionIncludesValues(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	s := vaultReadySubject(t, srv, reg)

	// GET with no vault: 404.
	rec := doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// The internal GET returns the FULL revision including source values —
	// exactly what the server merges against for a PATCH.
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":1`)
	assert.Contains(t, rec.Body.String(), "sekret")
	assert.Contains(t, rec.Body.String(), `"type":"inline"`)

	_ = s
}

func TestVaultRelayDeleteKeepsPolicy(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	s := vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	rec = doVault(t, srv, http.MethodDelete, vaultTestUID, "", "")
	require.Equal(t, http.StatusNoContent, rec.Code)

	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusNotFound, rec.Code)

	// The policy is kept: the subject stays active with its binding policy.
	state, ok := reg.Get(s)
	require.True(t, ok)
	assert.Equal(t, subject.StateActive, state)
	assert.NotNil(t, reg.UserPolicy(s))

	// Duplicate DELETE is a 404 (sidecar ErrNotFound parity).
	rec = doVault(t, srv, http.MethodDelete, vaultTestUID, "", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestVaultRelayIncarnationPrecondition(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	vaultReadySubject(t, srv, reg) // registers fence runtime-1 (testFenceR)

	// Matching incarnation on a mutation.
	rec := doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), testFenceR)
	require.Equal(t, http.StatusCreated, rec.Code)

	// Mismatch is a 412 — a stale writer must never land on a reset's successor.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("1"), "runtime-999")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// GET is validated too.
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "runtime-999")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", testFenceR)
	require.Equal(t, http.StatusOK, rec.Code)

	// Absent means unconditional.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("1"), "")
	require.Equal(t, http.StatusOK, rec.Code)

	// The incarnation is opaque: any value that does not match the registered
	// runtime instance is a 412, never silently ignored.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("2"), "not-an-incarnation")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)
}

func TestVaultRelaySharesActiveVaultStore(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// The mitm addon's active-vault socket resolves the SAME per-subject
	// store by client source IP: the relayed revision is what flows inject.
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil)
	rec2 := httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Contains(t, rec2.Body.String(), "Bearer sekret")
}

func TestVaultRelayMethodNotAllowed(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPatch, vaultTestUID, "{}", "")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Contains(t, rec.Header().Get("Allow"), "PUT")

	// PATCH composes server-side (relay GET → merge → relay PUT); egress
	// deliberately does not offer it.
	rec = doVault(t, srv, http.MethodPatch, vaultTestUID, `{"credentials":{}}`, "")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

// ---------------------------------------------------------------------------
// Fence binding (the vault slice must never outlive its runtime instance)
// ---------------------------------------------------------------------------

// TestVaultRelayRebindDiscardsPreviousVault: same UID rebinds under a new
// runtime-instance fence without REMOVE_BINDING — the old vault must be gone
// from both the relay surface and the addon's client-IP dispatch.
func TestVaultRelayRebindDiscardsPreviousVault(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	s := vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// Rebind: same UID, same slot IP, NEW runtime instance — no REMOVE_BINDING.
	const newRuntime = "runtime-2"
	rec = doAction(t, srv, bindBody(t, vaultTestUID, vaultTestIP, newRuntime, "attachment-2", 2, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, vaultTestUID, newRuntime, "attachment-2", constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, vaultTestUID, newRuntime, "attachment-2", constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)

	// The old vault is gone: relay GET is a 404 (fresh store), and the addon
	// dispatch for the (reused) slot IP injects nothing.
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil)
	rec2 := httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec2, req)
	require.Equal(t, http.StatusNotFound, rec2.Code)

	// The old incarnation cannot write into the successor.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, vaultPut("1"), testFenceR)
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// The successor installs its own vault cleanly.
	rec = doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), newRuntime)
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, subject.StateActive, mustState(reg.Get(s)))
}

// An absent subject never materializes a store. Explicitly seed legacy
// zero-fence state as well to retain defensive orphan cleanup coverage.
func TestVaultForResetsOrphanMaterializedWhileUnregistered(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	s := subject.FromSandboxUID(vaultTestUID)

	require.Nil(t, srv.vaultFor(s))
	require.Empty(t, srv.vaults)
	// Legacy orphan: bindings would require an effective policy.
	orphan := credentialvault.NewStore(nil, func() bool { return true })
	srv.vaults[s] = vaultEntry{store: orphan}
	_, err := orphan.Create(credentialvault.CreateRequest{
		Credentials: []credentialvault.Credential{
			{Name: "tok", Source: json.RawMessage(`{"type":"inline","value":"orphan-secret"}`)},
		},
	}, nil)
	require.NoError(t, err)

	// The successor registers; the orphan must not surface.
	vaultReadySubject(t, srv, reg)
	rec := doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusNotFound, rec.Code, "an orphaned store must never be inherited across registration")

	// And the live store is not the orphan: a push lands on a fresh store.
	assert.NotSame(t, orphan, srv.vaultFor(s))
	rec = doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "orphan-secret")
}

// TestVaultRelayDenyingSubjectValidatesAgainstPendingPolicy: a vault op
// between SET_BINDING and data-plane-ready is accepted (held to the barrier)
// and validates against the pending binding policy — not against nil.
func TestVaultRelayDenyingSubjectValidatesAgainstPendingPolicy(t *testing.T) {
	srv, _, _ := fastSandboxTestServer(t)

	// SET_BINDING only: subject registered, still denying, no effective policy.
	rec := doAction(t, srv, bindBody(t, vaultTestUID, vaultTestIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doAction(t, srv, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookRuntimeReady))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code, "a denying subject validates against its pending binding policy")

	// A binding the pending policy does not cover still fails coverage.
	bad := strings.Replace(vaultCredential(), "api.example.com", "other.example.com", 1)
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, bad, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// data-plane-ready activates; the held vault is live for flows.
	rec = doAction(t, srv, hookBody(t, vaultTestUID, testFenceR, testFenceA, constants.HookDataPlaneReady))
	require.Equal(t, http.StatusOK, rec.Code)
	req := httptest.NewRequest(http.MethodGet, "/credential-vault/_active?clientIp="+vaultTestIP, nil)
	rec2 := httptest.NewRecorder()
	srv.handleCredentialVaultActive(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Contains(t, rec2.Body.String(), "Bearer sekret")
}

// ---------------------------------------------------------------------------
// GET → PUT round-trip (revision tolerated as the precondition)
// ---------------------------------------------------------------------------

func TestVaultRelayGetPutRoundTrip(t *testing.T) {
	srv, reg, _ := fastSandboxTestServer(t)
	vaultReadySubject(t, srv, reg)

	rec := doVault(t, srv, http.MethodPost, vaultTestUID, vaultCredential(), "")
	require.Equal(t, http.StatusCreated, rec.Code)

	// GET the full revision, PUT it straight back: the "revision" field acts
	// as the precondition.
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	full := rec.Body.String()
	assert.Contains(t, full, `"revision":1`)

	rec = doVault(t, srv, http.MethodPut, vaultTestUID, full, "")
	require.Equal(t, http.StatusOK, rec.Code, "the GET body must be re-pushable as a replace body")
	assert.Contains(t, rec.Body.String(), `"revision":2`)

	// Stale read-revision: 412.
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, full, "")
	require.Equal(t, http.StatusPreconditionFailed, rec.Code)

	// expectedRevision wins when both agree; differing values are ambiguous.
	rec = doVault(t, srv, http.MethodGet, vaultTestUID, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	fresh := rec.Body.String()
	require.Contains(t, fresh, `"revision":2`)
	both := strings.Replace(fresh, `"revision":2`, `"revision":2,"expectedRevision":2`, 1)
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, both, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revision":3`)

	ambig := strings.Replace(fresh, `"revision":2`, `"revision":1,"expectedRevision":3`, 1)
	rec = doVault(t, srv, http.MethodPut, vaultTestUID, ambig, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------------------------------------------------------------------
// Path-derived subject ID validation
// ---------------------------------------------------------------------------

func TestValidVaultSubjectID(t *testing.T) {
	valid := []string{"u-1", "s-8f14e45f", strings.Repeat("a", 128), "...", "a..b"}
	for _, uid := range valid {
		assert.True(t, validVaultSubjectID(uid), uid)
	}
	invalid := []string{"", ".", "..", "a/b", "a\\b", "a\x00b", "a\nb", "a\x1bb", "a\x7fb", strings.Repeat("a", 129)}
	for _, uid := range invalid {
		assert.False(t, validVaultSubjectID(uid), uid)
	}
}
