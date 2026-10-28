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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/alibaba/opensandbox/egress/pkg/credentialvault"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

var (
	errInvalidEffectivePolicyCandidate = errors.New("invalid effective policy candidate")
	errStaleEffectivePolicyCandidate   = errors.New("stale effective policy candidate base")
)

// effectivePolicyInputs is explicit to avoid reentering AlwaysRuleLoader from
// its locked reload callback. Telemetry must already be resolved by the caller;
// neither construction nor preparation reads a loader, environment, or proxy.
// Nameserver grants belong to later nft-only derivation, never this policy.
type effectivePolicyInputs struct {
	user           *policy.NetworkPolicy
	alwaysDeny     []policy.EgressRule
	alwaysAllow    []policy.EgressRule
	telemetryAllow []policy.EgressRule
}

// effectivePolicyBase is an immutable baseline handle, independent of Vault
// revision and policy content. Every replacement must construct a fresh handle,
// even if epoch/content repeat. It is deliberately NOT a live publication owner.
// The mutation owner retains the authoritative current handle and uses it to
// validate candidates under the shared policy/Vault barrier. Validating against
// a candidate's saved old handle cannot establish that the policy is current.
type effectivePolicyBase struct {
	inputs effectivePolicyInputs
	epoch  int64
}

type effectivePolicyCandidate struct {
	base      *effectivePolicyBase
	store     *credentialvault.Store
	vault     *credentialvault.PolicySnapshot
	inputs    effectivePolicyInputs
	effective *policy.NetworkPolicy
	epoch     int64
	payload   []byte
	digest    string
}

func (effectivePolicyCandidate) String() string   { return "effectivePolicyCandidate" }
func (effectivePolicyCandidate) GoString() string { return "effectivePolicyCandidate{}" }

func newEffectivePolicyBase(inputs effectivePolicyInputs, epoch int64) (*effectivePolicyBase, error) {
	if epoch < 0 {
		return nil, errInvalidEffectivePolicyCandidate
	}
	frozen, err := freezeEffectivePolicyInputs(inputs)
	if err != nil {
		return nil, err
	}
	return &effectivePolicyBase{inputs: frozen, epoch: epoch}, nil
}

// Prepare freezes an unpublished prospective policy epoch and the unchanged
// current Vault. It never changes the base, Store, proxy, files, or nft state.
// The caller must hold the shared mutation barrier while obtaining the current
// base/inputs and preparing/validating a candidate. No external effects or active
// epoch publication are implemented here, and there is intentionally no Commit.
// Identical inputs still produce base+1: deciding whether a change is a no-op
// belongs to the publication owner. Preparing again does not reserve or
// consume another epoch, and yields the same bytes for the same inputs/Vault.
func (b *effectivePolicyBase) Prepare(store *credentialvault.Store, next effectivePolicyInputs) (*effectivePolicyCandidate, error) {
	if b == nil || store == nil || b.epoch < 0 || b.epoch == math.MaxInt64 {
		return nil, errInvalidEffectivePolicyCandidate
	}
	frozen, err := freezeEffectivePolicyInputs(next)
	if err != nil {
		return nil, err
	}
	allow := append(append([]policy.EgressRule(nil), frozen.alwaysAllow...), frozen.telemetryAllow...)
	effective := policy.MergeAlwaysOverlay(frozen.user, frozen.alwaysDeny, allow)
	vault, err := store.FreezeForPolicy(effective)
	if err != nil {
		return nil, err
	}
	epoch := b.epoch + 1
	payload, err := marshalEffectivePolicySnapshot(vault.Snapshot(), epoch)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	return &effectivePolicyCandidate{base: b, store: store, vault: vault, inputs: frozen, effective: effective, epoch: epoch, payload: payload, digest: hex.EncodeToString(digest[:])}, nil
}

// Validate requires the authoritative current baseline and Store, not merely
// the candidate's original base/Store. It does not reserve state after returning.
func (b *effectivePolicyBase) Validate(store *credentialvault.Store, candidate *effectivePolicyCandidate) error {
	if b == nil || candidate == nil || candidate.base == nil || candidate.store == nil || candidate.vault == nil {
		return errInvalidEffectivePolicyCandidate
	}
	if candidate.base != b {
		return errStaleEffectivePolicyCandidate
	}
	if store == nil || store != candidate.store {
		return credentialvault.ErrInvalidCandidate
	}
	return store.ValidatePolicySnapshot(candidate.vault)
}

func (c *effectivePolicyCandidate) BaseEpoch() int64   { return c.base.epoch }
func (c *effectivePolicyCandidate) PolicyEpoch() int64 { return c.epoch }
func (c *effectivePolicyCandidate) Exists() bool       { return c.vault.Exists() }
func (c *effectivePolicyCandidate) Snapshot() credentialvault.ActiveSnapshot {
	return c.vault.Snapshot()
}
func (c *effectivePolicyCandidate) Payload() []byte { return append([]byte(nil), c.payload...) }

// Digest authenticates only the canonical decision payload (Vault and epoch),
// not these policy inputs. Different candidates from one base may share a digest.
// The effect owner must associate its effects with the exact candidate;
// this foundation does not reserve an epoch or authenticate a full policy.
func (c *effectivePolicyCandidate) Digest() string { return c.digest }
func (c *effectivePolicyCandidate) Inputs() effectivePolicyInputs {
	return cloneEffectivePolicyInputs(c.inputs)
}
func (c *effectivePolicyCandidate) EffectivePolicy() *policy.NetworkPolicy {
	return policy.MergeAlwaysOverlay(c.effective, nil, nil)
}

func cloneEffectivePolicyInputs(inputs effectivePolicyInputs) effectivePolicyInputs {
	return effectivePolicyInputs{
		user:           policy.MergeAlwaysOverlay(inputs.user, nil, nil),
		alwaysDeny:     append([]policy.EgressRule(nil), inputs.alwaysDeny...),
		alwaysAllow:    append([]policy.EgressRule(nil), inputs.alwaysAllow...),
		telemetryAllow: append([]policy.EgressRule(nil), inputs.telemetryAllow...),
	}
}

func freezeEffectivePolicyInputs(inputs effectivePolicyInputs) (effectivePolicyInputs, error) {
	// Reparse copied public fields so a mutable CurrentPolicy pointer's compiled
	// index can never disagree with the frozen rules used for validation.
	raw, err := json.Marshal(inputs.user)
	if err != nil {
		return effectivePolicyInputs{}, errInvalidEffectivePolicyCandidate
	}
	user, err := policy.ParsePolicy(string(raw))
	if err != nil {
		return effectivePolicyInputs{}, fmt.Errorf("%w: parse user policy: %w", errInvalidEffectivePolicyCandidate, err)
	}
	if user.DefaultAction != policy.ActionAllow && user.DefaultAction != policy.ActionDeny {
		return effectivePolicyInputs{}, fmt.Errorf("%w: user policy: unsupported default action %q", errInvalidEffectivePolicyCandidate, user.DefaultAction)
	}
	freezeRules := func(name string, rules []policy.EgressRule, action string) ([]policy.EgressRule, error) {
		out := make([]policy.EgressRule, len(rules))
		for i, rule := range rules {
			if len(rule.PortRanges) > 0 {
				// Floor rules: already-validated intervals with no target
				// string to re-parse; freeze as-is.
				if rule.Action != action {
					return nil, fmt.Errorf("%w: %s rule %d: action %q, want %q", errInvalidEffectivePolicyCandidate, name, i, rule.Action, action)
				}
				out[i] = rule
				continue
			}
			parsed, err := policy.ParseValidatedEgressRule(rule.Action, rule.Target)
			if err != nil {
				return nil, fmt.Errorf("%w: %s rule %d: %w", errInvalidEffectivePolicyCandidate, name, i, err)
			}
			if parsed.Action != action {
				return nil, fmt.Errorf("%w: %s rule %d: action %q, want %q", errInvalidEffectivePolicyCandidate, name, i, parsed.Action, action)
			}
			out[i] = parsed
		}
		return out, nil
	}
	deny, err := freezeRules("alwaysDeny", inputs.alwaysDeny, policy.ActionDeny)
	if err != nil {
		return effectivePolicyInputs{}, err
	}
	allow, err := freezeRules("alwaysAllow", inputs.alwaysAllow, policy.ActionAllow)
	if err != nil {
		return effectivePolicyInputs{}, err
	}
	telemetry, err := freezeRules("telemetryAllow", inputs.telemetryAllow, policy.ActionAllow)
	if err != nil {
		return effectivePolicyInputs{}, err
	}
	return effectivePolicyInputs{user: user, alwaysDeny: deny, alwaysAllow: allow, telemetryAllow: telemetry}, nil
}

// marshalEffectivePolicySnapshot preserves the fixed validation sentinel and
// stage without adding credential-bearing snapshot or payload diagnostics.
func marshalEffectivePolicySnapshot(snapshot credentialvault.ActiveSnapshot, epoch int64) ([]byte, error) {
	payload, err := credentialvault.MarshalDecisionSnapshot(snapshot, epoch)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal decision snapshot: %w", errInvalidEffectivePolicyCandidate, err)
	}
	return payload, nil
}
