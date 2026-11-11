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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMutationCandidateCreateIsInvisibleUntilCommit(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)

	request := testCredentialVaultRequest()
	candidate, err := store.PrepareCreate(request, policy)
	require.NoError(t, err)
	request.Bindings[0].Match.Hosts[0] = "caller-mutated-before-render.example.com"
	_, err = store.Sanitized()
	require.ErrorIs(t, err, ErrNotFound)

	state, err := candidate.Sanitized()
	require.NoError(t, err)
	require.Equal(t, int64(1), state.Revision)
	require.NotContains(t, fmt.Sprintf("%v %#v", candidate, candidate), "secret-token")
	state.Bindings[0].Match.Hosts[0] = "caller-tampered.example.com"
	snapshot, err := candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), snapshot.Revision)
	require.Equal(t, "code.example.com", snapshot.Bindings[0].Match.Hosts[0])
	require.Equal(t, "secret-token", snapshot.Bindings[0].Headers[0].Value)
	request.Bindings[0].Match.Hosts[0] = "caller-mutated-after-render.example.com"
	state, err = candidate.Sanitized()
	require.NoError(t, err)

	committed, err := store.CommitCandidate(candidate)
	require.NoError(t, err)
	require.Equal(t, state, committed)
	require.Equal(t, "code.example.com", committed.Bindings[0].Match.Hosts[0])
	published, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, snapshot, published)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.ErrorIs(t, err, ErrCandidateClosed)
}

func TestMutationCandidatePatchRejectsConcurrentSameRevision(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), policy)
	require.NoError(t, err)
	expected := int64(1)
	candidate, err := store.PreparePatch(MutationRequest{
		ExpectedRevision: &expected,
		Credentials: &CredentialMutationSet{Replace: []Credential{{
			Name:   "gitlab-token",
			Source: mustMarshal(map[string]string{"type": "inline", "value": "candidate-secret"}),
		}}},
	}, policy)
	require.NoError(t, err)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)

	active, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Contains(t, active.Redactions, "secret-token")
	require.NotContains(t, active.Redactions, "candidate-secret")
	_, err = store.Patch(MutationRequest{
		ExpectedRevision: &expected,
		Credentials: &CredentialMutationSet{Replace: []Credential{{
			Name:   "gitlab-token",
			Source: mustMarshal(map[string]string{"type": "inline", "value": "concurrent-secret"}),
		}}},
	}, policy)
	require.NoError(t, err)

	_, err = store.CommitCandidate(candidate)
	require.ErrorIs(t, err, ErrStaleCandidate)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.ErrorIs(t, err, ErrCandidateClosed)
	active, err = store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, int64(2), active.Revision)
	require.Contains(t, active.Redactions, "concurrent-secret")
	require.NotContains(t, active.Redactions, "candidate-secret")
}

func TestMutationCandidateDeleteRejectsDeleteRecreateABA(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), policy)
	require.NoError(t, err)
	candidate, err := store.PrepareDelete()
	require.NoError(t, err)
	tombstone, err := candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, ActiveSnapshot{}, tombstone)
	_, err = store.Sanitized()
	require.NoError(t, err, "prepare delete must not publish")

	require.NoError(t, store.Delete())
	recreated := testCredentialVaultRequest()
	recreated.Credentials[0].Source = mustMarshal(map[string]string{
		"type": "inline", "value": "recreated-secret",
	})
	_, err = store.Create(recreated, policy)
	require.NoError(t, err)
	_, err = store.CommitCandidate(candidate)
	require.ErrorIs(t, err, ErrStaleCandidate)
	active, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, int64(1), active.Revision)
	require.Contains(t, active.Redactions, "recreated-secret")
}

func TestMutationCandidateDeleteCommitAndDiscard(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	_, err := store.Create(testCredentialVaultRequest(), policy)
	require.NoError(t, err)
	candidate, err := store.PrepareDelete()
	require.NoError(t, err)
	_, err = store.CommitCandidate(candidate)
	require.NoError(t, err)
	_, err = store.Sanitized()
	require.ErrorIs(t, err, ErrNotFound)

	candidate, err = store.PrepareCreate(testCredentialVaultRequest(), policy)
	require.NoError(t, err)
	candidate.Discard()
	_, err = store.CommitCandidate(candidate)
	require.ErrorIs(t, err, ErrCandidateClosed)
}

func TestMutationCandidateMustRenderBeforeCommit(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	candidate, err := store.PrepareCreate(testCredentialVaultRequest(), policy)
	require.NoError(t, err)
	_, err = store.CommitCandidate(candidate)
	require.ErrorIs(t, err, ErrCandidateNotRendered)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	other := NewStore(nil, func() bool { return true })
	_, err = other.CommitCandidate(candidate)
	require.ErrorIs(t, err, ErrInvalidCandidate)
	_, err = store.CommitCandidate(candidate)
	require.NoError(t, err)
}

type rotatingCandidateSource struct {
	resolves *atomic.Int32
}

type failingCandidateSource struct{}

func (failingCandidateSource) Type() string { return "failing-candidate" }

func (failingCandidateSource) Resolve(context.Context) (string, error) {
	return "", errors.New("source unavailable")
}

func (s *rotatingCandidateSource) Type() string { return "rotating-candidate" }

func (s *rotatingCandidateSource) Resolve(context.Context) (string, error) {
	return fmt.Sprintf("resolved-secret-%d", s.resolves.Add(1)), nil
}

func TestMutationCandidateFreezesRenderedSnapshot(t *testing.T) {
	var resolves atomic.Int32
	registry := NewSourceRegistry()
	registry.Register("rotating-candidate", func(json.RawMessage) (CredentialSource, error) {
		return &rotatingCandidateSource{resolves: &resolves}, nil
	})
	store := NewStoreWithRegistry(nil, func() bool { return true }, registry)
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	request := testCredentialVaultRequest()
	request.Credentials[0].Source = json.RawMessage(`{"type":"rotating-candidate"}`)
	candidate, err := store.PrepareCreate(request, policy)
	require.NoError(t, err)

	first, err := candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	second, err := candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, int32(1), resolves.Load())
	first.Bindings[0].Headers[0].Value = "caller-tampered"
	third, err := candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	require.Equal(t, "resolved-secret-1", third.Bindings[0].Headers[0].Value)

	_, err = store.CommitCandidate(candidate)
	require.NoError(t, err)
	published, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, third, published)
	require.Equal(t, int32(1), resolves.Load())
	published.Bindings[0].Headers[0].Value = "published-caller-tampered"
	published, err = store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, "resolved-secret-1", published.Bindings[0].Headers[0].Value)
	require.Equal(t, int32(1), resolves.Load())
}

func TestMutationCandidateCommitsAtMostOnceConcurrently(t *testing.T) {
	store := NewStore(nil, func() bool { return true })
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	candidate, err := store.PrepareCreate(testCredentialVaultRequest(), policy)
	require.NoError(t, err)
	_, err = candidate.ActiveSnapshot(context.Background())
	require.NoError(t, err)

	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := store.CommitCandidate(candidate)
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	if first == nil {
		require.ErrorIs(t, second, ErrCandidateClosed)
	} else {
		require.ErrorIs(t, first, ErrCandidateClosed)
		require.NoError(t, second)
	}
}

func TestActiveSnapshotRenderErrorPreservesEmptyChangeMetadata(t *testing.T) {
	registry := NewSourceRegistry()
	registry.Register("failing-candidate", func(json.RawMessage) (CredentialSource, error) {
		return failingCandidateSource{}, nil
	})
	store := NewStoreWithRegistry(nil, func() bool { return true }, registry)
	policy := testCredentialPolicy(t, `{"defaultAction":"deny","egress":[{"action":"allow","target":"code.example.com"}]}`)
	request := testCredentialVaultRequest()
	request.Credentials[0].Source = json.RawMessage(`{"type":"failing-candidate"}`)
	_, err := store.Create(request, policy)
	require.NoError(t, err)

	snapshot, tag, changed, err := store.ActiveSnapshotIfChanged(context.Background(), "")
	require.EqualError(t, err, "source unavailable")
	require.Equal(t, ActiveSnapshot{}, snapshot)
	require.Empty(t, tag)
	require.False(t, changed)
}
