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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/require"
)

func policyCandidateInputs(t *testing.T) effectivePolicyInputs {
	t.Helper()
	pol, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"api.example.com"}]}`)
	require.NoError(t, err)
	return effectivePolicyInputs{user: pol}
}

func policyCandidateStore(t *testing.T, inputs effectivePolicyInputs) *credentialvault.Store {
	t.Helper()
	store := credentialvault.NewStore(nil, nil)
	mutation, err := store.PrepareCreate(integrationVaultRequest("private-policy-candidate"), inputs.user)
	require.NoError(t, err)
	_, err = mutation.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	_, err = store.CommitCandidate(mutation)
	require.NoError(t, err)
	return store
}

func TestEffectivePolicyCandidateFreezesInputsAndPayload(t *testing.T) {
	inputs := policyCandidateInputs(t)
	store := policyCandidateStore(t, inputs)
	base, err := newEffectivePolicyBase(inputs, 7)
	require.NoError(t, err)
	next := policyCandidateInputs(t)
	next.alwaysAllow = []policy.EgressRule{{Action: "allow", Target: "always.example.com"}}
	next.telemetryAllow = []policy.EgressRule{{Action: "allow", Target: "telemetry.example.com"}}
	candidate, err := base.Prepare(store, next)
	require.NoError(t, err)
	before, err := store.ActiveSnapshot()
	require.NoError(t, err)
	require.Equal(t, before, candidate.Snapshot())
	require.True(t, candidate.Exists())
	require.Equal(t, int64(7), candidate.BaseEpoch())
	require.Equal(t, int64(8), candidate.PolicyEpoch())
	require.Equal(t, int64(7), base.epoch)
	payload, err := credentialvault.MarshalDecisionSnapshot(before, 8)
	require.NoError(t, err)
	require.Equal(t, payload, candidate.Payload())
	sum := sha256.Sum256(payload)
	require.Equal(t, hex.EncodeToString(sum[:]), candidate.Digest())
	inputs.user.Egress[0].Target = "mutated-base.example.com"
	next.user.Egress[0].Target = "mutated-next.example.com"
	next.alwaysAllow[0].Target = "mutated-allow.example.com"
	next.telemetryAllow[0].Target = "mutated-telemetry.example.com"
	exposed := candidate.Inputs()
	exposed.user.Egress[0].Target = "mutated-output.example.com"
	exposed.alwaysAllow[0].Target = "mutated-output.example.com"
	candidate.Payload()[0] = '!'
	changed := candidate.Snapshot()
	changed.Bindings[0].Headers[0].Value = "tampered"
	effective := candidate.EffectivePolicy()
	effective.Egress[0].Target = "mutated-effective.example.com"
	require.Equal(t, "api.example.com", candidate.Inputs().user.Egress[0].Target)
	require.Equal(t, "always.example.com", candidate.Inputs().alwaysAllow[0].Target)
	require.Equal(t, "telemetry.example.com", candidate.Inputs().telemetryAllow[0].Target)
	require.Equal(t, policy.ActionAllow, candidate.EffectivePolicy().Evaluate("api.example.com"))
	require.Equal(t, policy.ActionAllow, candidate.EffectivePolicy().Evaluate("telemetry.example.com"))
	require.Equal(t, payload, candidate.Payload())
	require.Equal(t, before, candidate.Snapshot())
	require.NoError(t, base.Validate(store, candidate))
	require.Equal(t, "effectivePolicyCandidate effectivePolicyCandidate{} effectivePolicyCandidate effectivePolicyCandidate{}", fmt.Sprintf("%v %#v %v %#v", candidate, candidate, *candidate, *candidate))
}

func TestEffectivePolicyCandidateRejectsStaleBasesAndVaultABA(t *testing.T) {
	inputs := policyCandidateInputs(t)
	store := policyCandidateStore(t, inputs)
	base, err := newEffectivePolicyBase(inputs, 0)
	require.NoError(t, err)
	candidate, err := base.Prepare(store, inputs)
	require.NoError(t, err)
	replacement, err := newEffectivePolicyBase(inputs, 0)
	require.NoError(t, err)
	require.ErrorIs(t, replacement.Validate(store, candidate), errStaleEffectivePolicyCandidate)
	require.NoError(t, base.Validate(store, candidate))
	require.ErrorIs(t, base.Validate(credentialvault.NewStore(nil, nil), candidate), credentialvault.ErrInvalidCandidate)
	require.NoError(t, store.Delete())
	_, err = store.Create(integrationVaultRequest("recreated"), inputs.user)
	require.NoError(t, err)
	require.ErrorIs(t, base.Validate(store, candidate), credentialvault.ErrStaleCandidate)
}

func TestEffectivePolicyCandidateOrderedOverlays(t *testing.T) {
	inputs := policyCandidateInputs(t)
	store := policyCandidateStore(t, inputs)
	base, err := newEffectivePolicyBase(inputs, 2)
	require.NoError(t, err)
	next := effectivePolicyInputs{user: policy.DefaultDenyPolicy(), telemetryAllow: []policy.EgressRule{{Action: "allow", Target: "api.example.com"}}}
	candidate, err := base.Prepare(store, next)
	require.NoError(t, err)
	require.NoError(t, base.Validate(store, candidate))
	next.alwaysDeny = []policy.EgressRule{{Action: "deny", Target: "api.example.com"}}
	_, err = base.Prepare(store, next)
	require.Error(t, err)
	next.alwaysDeny = nil
	next.telemetryAllow = nil
	_, err = base.Prepare(store, next)
	require.Error(t, err, "nameserver grants cannot serve as credential permission")
	next = policyCandidateInputs(t)
	next.user.Egress = append([]policy.EgressRule{{Action: "deny", Target: "api.example.com"}}, next.user.Egress...)
	_, err = base.Prepare(store, next)
	require.Error(t, err, "fresh compilation must respect changed first-match order")
	next.alwaysAllow = []policy.EgressRule{{Action: "allow", Target: "api.example.com"}}
	_, err = base.Prepare(store, next)
	require.NoError(t, err)
}

func TestEffectivePolicyCandidateInvalidEpochAndInputs(t *testing.T) {
	inputs := policyCandidateInputs(t)
	_, err := newEffectivePolicyBase(inputs, -1)
	require.Error(t, err)
	base, err := newEffectivePolicyBase(inputs, math.MaxInt64)
	require.NoError(t, err)
	_, err = base.Prepare(credentialvault.NewStore(nil, nil), inputs)
	require.Error(t, err)
	inputs.user.DefaultAction = "unknown"
	_, err = newEffectivePolicyBase(inputs, 0)
	require.Error(t, err)
	inputs = policyCandidateInputs(t)
	inputs.alwaysDeny = []policy.EgressRule{{Action: "allow", Target: "example.com"}}
	_, err = newEffectivePolicyBase(inputs, 0)
	require.Error(t, err)
}

func TestEffectivePolicyCandidateConcurrentReadAndVaultMutation(t *testing.T) {
	inputs := policyCandidateInputs(t)
	store := policyCandidateStore(t, inputs)
	base, err := newEffectivePolicyBase(inputs, 4)
	require.NoError(t, err)
	candidate, err := base.Prepare(store, inputs)
	require.NoError(t, err)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 50 {
				_ = candidate.Inputs()
				_ = candidate.Payload()
				_ = candidate.Snapshot()
				_ = base.Validate(store, candidate)
			}
		}()
	}
	_, err = store.Patch(credentialvault.MutationRequest{}, inputs.user)
	require.NoError(t, err)
	readers.Wait()
	require.ErrorIs(t, base.Validate(store, candidate), credentialvault.ErrStaleCandidate)
}

func TestEffectivePolicyCandidateNoopIsProspectiveOnly(t *testing.T) {
	inputs := policyCandidateInputs(t)
	base, err := newEffectivePolicyBase(inputs, 12)
	require.NoError(t, err)
	for _, exists := range []bool{false, true} {
		store := credentialvault.NewStore(nil, nil)
		if exists {
			_, err = store.Create(credentialvault.CreateRequest{}, inputs.user)
			require.NoError(t, err)
		}
		first, err := base.Prepare(store, inputs)
		require.NoError(t, err)
		second, err := base.Prepare(store, inputs)
		require.NoError(t, err)
		require.Equal(t, int64(13), first.PolicyEpoch())
		require.Equal(t, first.Payload(), second.Payload())
		require.Equal(t, first.Digest(), second.Digest())
		require.Equal(t, exists, first.Exists())
		require.Equal(t, int64(12), base.epoch)
		require.NoError(t, base.Validate(store, first))
		require.NoError(t, base.Validate(store, second))
	}
}

func TestEffectivePolicyCandidateDigestIsDecisionPayloadOnly(t *testing.T) {
	inputs := policyCandidateInputs(t)
	base, err := newEffectivePolicyBase(inputs, 3)
	require.NoError(t, err)
	store := policyCandidateStore(t, inputs)
	first, err := base.Prepare(store, inputs)
	require.NoError(t, err)
	next := policyCandidateInputs(t)
	next.alwaysAllow = []policy.EgressRule{{Action: "allow", Target: "unbound.example.com"}}
	second, err := base.Prepare(store, next)
	require.NoError(t, err)
	require.NotEqual(t, first.Inputs(), second.Inputs())
	require.Equal(t, first.Payload(), second.Payload())
	require.Equal(t, first.Digest(), second.Digest())
	require.NoError(t, base.Validate(store, first))
	require.NoError(t, base.Validate(store, second))
	require.Equal(t, int64(3), base.epoch)
}

func TestEffectivePolicyCandidatePreparePropagatesValidationContext(t *testing.T) {
	inputs := policyCandidateInputs(t)
	var err error
	inputs.user, err = policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"}]}`)
	require.NoError(t, err)
	store := credentialvault.NewStore(nil, nil)
	request := integrationVaultRequest("private-policy-candidate")
	request.Bindings[0].Match.Hosts = []string{"*.example.com"}
	mutation, err := store.PrepareCreate(request, inputs.user)
	require.NoError(t, err)
	snapshot, err := mutation.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	state, err := store.CommitCandidate(mutation)
	require.NoError(t, err)
	base, err := newEffectivePolicyBase(inputs, 7)
	require.NoError(t, err)
	valid, err := base.Prepare(store, inputs)
	require.NoError(t, err)
	for _, tc := range []struct{ policy, reason string }{
		{`{"defaultAction":"deny"}`, `binding "api" host "*.example.com" is not allowed by egress policy`},
		{`{"defaultAction":"deny","egress":[{"action":"deny","target":"private.example.com"},{"action":"allow","target":"*.example.com"}]}`, `binding "api" host "*.example.com" is not entirely allowed by egress policy`},
	} {
		next := policyCandidateInputs(t)
		next.user, err = policy.ParsePolicy(tc.policy)
		require.NoError(t, err)
		rejected, err := base.Prepare(store, next)
		require.Nil(t, rejected)
		require.ErrorIs(t, err, credentialvault.ErrInvalidCandidate)
		require.Contains(t, err.Error(), tc.reason)
		require.NotContains(t, err.Error(), "private-policy-candidate")
		require.NoError(t, base.Validate(store, valid))
		require.Equal(t, int64(7), base.epoch)
		after, stateErr := store.Sanitized()
		require.NoError(t, stateErr)
		require.Equal(t, state, after)
		active, snapshotErr := store.ActiveSnapshot()
		require.NoError(t, snapshotErr)
		require.Equal(t, snapshot, active)
	}
}

func TestEffectivePolicyInputDiagnostics(t *testing.T) {
	inputs := policyCandidateInputs(t)
	var resolves atomic.Int32
	registry := credentialvault.NewSourceRegistry()
	registry.Register("diagnostic", func(json.RawMessage) (credentialvault.CredentialSource, error) {
		return &diagnosticCredentialSource{calls: &resolves}, nil
	})
	store := credentialvault.NewStoreWithRegistry(nil, nil, registry)
	request := integrationVaultRequest("private-policy-candidate")
	request.Credentials[0].Source = json.RawMessage(`{"type":"diagnostic"}`)
	mutation, err := store.PrepareCreate(request, inputs.user)
	require.NoError(t, err)
	_, err = mutation.ActiveSnapshot(context.Background())
	require.NoError(t, err)
	_, err = store.CommitCandidate(mutation)
	require.NoError(t, err)
	base, err := newEffectivePolicyBase(inputs, 4)
	require.NoError(t, err)
	valid, err := base.Prepare(store, inputs)
	require.NoError(t, err)
	state, err := store.Sanitized()
	require.NoError(t, err)
	snapshot, err := store.ActiveSnapshot()
	require.NoError(t, err)
	cases := []struct {
		name      string
		mutate    func(*effectivePolicyInputs)
		fragments []string
		cause     bool
	}{
		{"user action", func(in *effectivePolicyInputs) {
			in.user.Egress[0].Action = "invalid"
			in.user.Egress[0].Target = "raw-target-private.example.com"
		}, []string{"parse user policy", `unsupported action "invalid"`}, true},
		{"user empty target", func(in *effectivePolicyInputs) { in.user.Egress[0].Target = "" }, []string{"parse user policy", "egress target cannot be empty"}, true},
		{"user default", func(in *effectivePolicyInputs) { in.user.DefaultAction = "invalid" }, []string{"user policy", `unsupported default action "invalid"`}, false},
	}
	for _, list := range []string{"alwaysDeny", "alwaysAllow", "telemetryAllow"} {
		for _, failure := range []string{"parse action", "empty target", "wrong action"} {
			list, failure := list, failure
			expected := "allow"
			if list == "alwaysDeny" {
				expected = "deny"
			}
			fragments := []string{list, "rule 1"}
			switch failure {
			case "parse action":
				fragments = append(fragments, `unsupported action "invalid"`)
			case "empty target":
				fragments = append(fragments, "egress target cannot be empty")
			case "wrong action":
				fragments = append(fragments, "action", `want "`+expected+`"`)
			}
			cases = append(cases, struct {
				name      string
				mutate    func(*effectivePolicyInputs)
				fragments []string
				cause     bool
			}{list + " " + failure, func(in *effectivePolicyInputs) {
				rules := []policy.EgressRule{{Action: expected, Target: "first.example.com"}, {Action: expected, Target: "raw-target-private.example.com"}}
				switch failure {
				case "parse action":
					rules[1].Action = "invalid"
				case "empty target":
					rules[1].Target = ""
				case "wrong action":
					if expected == "allow" {
						rules[1].Action = "deny"
					} else {
						rules[1].Action = "allow"
					}
				}
				switch list {
				case "alwaysDeny":
					in.alwaysDeny = rules
				case "alwaysAllow":
					in.alwaysAllow = rules
				case "telemetryAllow":
					in.telemetryAllow = rules
				}
			}, fragments, failure != "wrong action"})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := policyCandidateInputs(t)
			tc.mutate(&next)
			rejected, err := base.Prepare(store, next)
			require.Nil(t, rejected)
			require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
			for _, part := range tc.fragments {
				require.Contains(t, err.Error(), part)
			}
			require.NotContains(t, err.Error(), "raw-target-private.example.com")
			require.NotContains(t, err.Error(), "private-policy-candidate")
			if tc.cause {
				wrapped, ok := err.(interface{ Unwrap() []error })
				require.True(t, ok)
				causes := wrapped.Unwrap()
				require.Len(t, causes, 2)
				require.ErrorIs(t, err, causes[1])
				require.NotEqual(t, errInvalidEffectivePolicyCandidate, causes[1])
			}
			require.NoError(t, base.Validate(store, valid))
			require.Equal(t, int64(4), base.epoch)
			after, e := store.Sanitized()
			require.NoError(t, e)
			require.Equal(t, state, after)
			active, e := store.ActiveSnapshot()
			require.NoError(t, e)
			require.Equal(t, snapshot, active)
			require.Equal(t, int32(1), resolves.Load(), "rejected policy input must not resolve credentials")
		})
	}
}

func TestMarshalEffectivePolicySnapshotDiagnostics(t *testing.T) {
	invalid := credentialvault.ActiveSnapshot{Revision: -1, Redactions: []string{"private-snapshot-secret"}}
	payload, err := marshalEffectivePolicySnapshot(invalid, 4)
	require.Nil(t, payload)
	require.ErrorIs(t, err, errInvalidEffectivePolicyCandidate)
	require.ErrorIs(t, err, credentialvault.ErrInvalidDecisionSnapshot)
	require.Contains(t, err.Error(), "marshal decision snapshot")
	require.NotContains(t, err.Error(), "private-snapshot-secret")
	require.NotContains(t, err.Error(), "redactions")
	snapshot := credentialvault.ActiveSnapshot{}
	got, err := marshalEffectivePolicySnapshot(snapshot, 4)
	require.NoError(t, err)
	expected, err := credentialvault.MarshalDecisionSnapshot(snapshot, 4)
	require.NoError(t, err)
	require.Equal(t, expected, got)
}

// A real registry source counts resolution without a mutable production seam.
type diagnosticCredentialSource struct{ calls *atomic.Int32 }

func (*diagnosticCredentialSource) Type() string { return "diagnostic" }
func (s *diagnosticCredentialSource) Resolve(context.Context) (string, error) {
	s.calls.Add(1)
	return "private-policy-candidate", nil
}
