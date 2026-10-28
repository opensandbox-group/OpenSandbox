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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// Relay-write transaction surface: PrepareReplace + candidate.Full for the
// commit protocol, Store.Reload for the recovery path.

func TestPrepareReplaceCreatesAndPreconditions(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	one := int64(1)

	// With no vault present the replace prepares a create at revision 1.
	candidate, err := store.PrepareReplace(ReplaceRequest{CreateRequest: testCredentialVaultRequest()}, pol)
	require.NoError(t, err)
	assert.True(t, candidate.Created())
	assert.True(t, candidate.Exists())
	_, err = store.Sanitized()
	assert.ErrorIs(t, err, ErrNotFound, "preparation must not publish")
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)

	state, err := store.CommitCandidate(candidate)
	require.NoError(t, err)
	assert.Equal(t, int64(1), state.Revision)

	// A matching precondition prepares the successor revision.
	candidate, err = store.PrepareReplace(ReplaceRequest{ExpectedRevision: &one, CreateRequest: testCredentialVaultRequest()}, pol)
	require.NoError(t, err)
	assert.False(t, candidate.Created())
	full, err := candidate.Full()
	require.NoError(t, err)
	assert.Equal(t, int64(2), full.Revision)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	_, err = store.CommitCandidate(candidate)
	require.NoError(t, err)

	// A stale precondition is rejected before anything durable happens.
	_, err = store.PrepareReplace(ReplaceRequest{ExpectedRevision: &one, CreateRequest: testCredentialVaultRequest()}, pol)
	assert.ErrorIs(t, err, ErrRevisionConflict)

	// An ambiguous precondition fails like Store.Replace.
	both := int64(2)
	_, err = store.PrepareReplace(ReplaceRequest{ExpectedRevision: &one, Revision: &both, CreateRequest: testCredentialVaultRequest()}, pol)
	assert.Error(t, err)
}

func TestPrepareReplaceCoverageFailureKeepsLastGood(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)

	uncovedHost := testCredentialVaultRequest()
	uncovedHost.Bindings[0].Match.Hosts[0] = "other.example.com"
	_, err = store.PrepareReplace(ReplaceRequest{CreateRequest: uncovedHost}, pol)
	assert.Error(t, err)

	full, err := store.Full()
	require.NoError(t, err)
	assert.Equal(t, int64(1), full.Revision, "a rejected replace never leaves a partially applied revision")
}

func TestMutationCandidateFullMirrorsStoreFull(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	candidate, err := store.PrepareCreate(testCredentialVaultRequest(), pol)
	require.NoError(t, err)

	candidateFull, err := candidate.Full()
	require.NoError(t, err)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	state, err := store.CommitCandidate(candidate)
	require.NoError(t, err)

	storeFull, err := store.Full()
	require.NoError(t, err)
	assert.Equal(t, storeFull, candidateFull, "the persisted payload must be exactly the committed revision")
	assert.Equal(t, state.Revision, candidateFull.Revision)
	require.Len(t, candidateFull.Credentials, 1)
	assert.NotEmpty(t, candidateFull.Credentials[0].Source, "the file payload includes values (plaintext v1 posture)")

	// A delete tombstone has no full revision.
	tombstone, err := store.PrepareDelete()
	require.NoError(t, err)
	_, err = tombstone.Full()
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = store.PrepareDelete()
	require.NoError(t, err)
}

// TestStoreReloadInstallsExactRevision: the recovery path installs the file
// revision verbatim and continues revision numbering from it — the continuity
// the relay preconditions rely on across an egress restart.
func TestStoreReloadInstallsExactRevision(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)

	body := VaultFileBody{
		Credentials: []Credential{{Name: "tok", Source: json.RawMessage(`{"type":"inline","value":"sekret"}`)}},
		Bindings: []Binding{{
			Name:  "api",
			Match: Match{Hosts: []string{"code.example.com"}},
			Auth:  Auth{Type: "bearer", Credential: "tok"},
		}},
	}
	require.NoError(t, store.Reload(7, CreateRequest(body), pol))

	full, err := store.Full()
	require.NoError(t, err)
	assert.Equal(t, int64(7), full.Revision)

	snapshot, err := store.ActiveSnapshotWithContext(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(7), snapshot.Revision)
	require.Len(t, snapshot.Bindings, 1)
	assert.Equal(t, "Bearer sekret", snapshot.Bindings[0].Headers[0].Value, "a reloaded vault is live for interception")

	// Numbering continues from the reloaded revision.
	_, created, err := store.Replace(ReplaceRequest{CreateRequest: CreateRequest(body)}, pol)
	require.NoError(t, err)
	assert.False(t, created)
	full, err = store.Full()
	require.NoError(t, err)
	assert.Equal(t, int64(8), full.Revision)
}

func TestStoreReloadValidationFailureKeepsPreviousContents(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	pol := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), pol)
	require.NoError(t, err)

	// A binding referencing an unknown credential is rejected, last-good kept.
	bad := CreateRequest{
		Credentials: []Credential{{Name: "tok", Source: json.RawMessage(`{"type":"inline","value":"x"}`)}},
		Bindings:    []Binding{{Name: "api", Match: Match{Hosts: []string{"code.example.com"}}, Auth: Auth{Type: "bearer", Credential: "ghost"}}},
	}
	err = store.Reload(9, bad, pol)
	assert.Error(t, err)
	full, err := store.Full()
	require.NoError(t, err)
	assert.Equal(t, int64(1), full.Revision)

	// Bindings without a policy are rejected too (fail closed).
	err = store.Reload(9, CreateRequest(body(t)), nil)
	assert.Error(t, err)

	// Non-positive revisions are rejected outright.
	err = store.Reload(0, CreateRequest{}, pol)
	assert.Error(t, err)
}

func body(t *testing.T) VaultFileBody {
	t.Helper()
	return VaultFileBody{
		Credentials: []Credential{{Name: "tok", Source: json.RawMessage(`{"type":"inline","value":"sekret"}`)}},
		Bindings: []Binding{{
			Name:  "api",
			Match: Match{Hosts: []string{"code.example.com"}},
			Auth:  Auth{Type: "bearer", Credential: "tok"},
		}},
	}
}

var _ = policy.ActionDeny
