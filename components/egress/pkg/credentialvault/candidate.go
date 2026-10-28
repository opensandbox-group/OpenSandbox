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
	"sort"
	"sync"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

var (
	ErrCandidateClosed      = errors.New("credential vault mutation candidate is closed")
	ErrCandidateNotRendered = errors.New("credential vault mutation candidate is not rendered")
	ErrStaleCandidate       = errors.New("credential vault mutation candidate is stale")
	ErrInvalidCandidate     = errors.New("invalid credential vault mutation candidate")
)

// MutationCandidate is one validated but unpublished Vault state. It may be
// rendered for a revision transaction and committed exactly once to its owner.
// Its internal maps and credential sources are never exposed directly.
type MutationCandidate struct {
	mu              sync.Mutex
	owner           *Store
	baseMutationTag string
	exists          bool
	created         bool
	revision        int64
	credentials     map[string]record
	bindings        map[string]Binding
	snapshot        *ActiveSnapshot
	closed          bool
}

func (*MutationCandidate) String() string { return "credentialvault.MutationCandidate" }

func (*MutationCandidate) GoString() string { return "credentialvault.MutationCandidate{}" }

// PrepareCreate validates a new Vault without publishing it.
func (v *Store) PrepareCreate(req CreateRequest, pol *policy.NetworkPolicy) (*MutationCandidate, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.exists {
		return nil, ErrExists
	}
	credentials, bindings, err := v.buildCreate(req, pol, 1)
	if err != nil {
		return nil, err
	}
	return v.newCandidate(true, 1, credentials, bindings), nil
}

// PreparePatch validates a replacement Vault revision without publishing it.
func (v *Store) PreparePatch(req MutationRequest, pol *policy.NetworkPolicy) (*MutationCandidate, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.exists {
		return nil, ErrNotFound
	}
	if req.ExpectedRevision != nil && *req.ExpectedRevision != v.revision {
		return nil, fmt.Errorf("%w: expectedRevision %d does not match current revision %d", ErrRevisionConflict, *req.ExpectedRevision, v.revision)
	}
	credentials, bindings, err := v.buildPatch(req, pol, v.revision+1)
	if err != nil {
		return nil, err
	}
	return v.newCandidate(true, v.revision+1, credentials, bindings), nil
}

// PrepareReplace validates a full-revision replace without publishing it —
// the relay PUT transaction: creates at revision 1 when no vault exists,
// otherwise swaps everything and advances the revision. The precondition
// follows Store.Replace; Created() reports whether committing would create.
func (v *Store) PrepareReplace(req ReplaceRequest, pol *policy.NetworkPolicy) (*MutationCandidate, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	expected, err := req.precondition()
	if err != nil {
		return nil, err
	}
	created := !v.exists
	if expected != nil && (created || *expected != v.revision) {
		return nil, fmt.Errorf("%w: expectedRevision %d does not match current revision %d", ErrRevisionConflict, *expected, v.revision)
	}
	next := int64(1)
	if !created {
		next = v.revision + 1
	}
	credentials, bindings, err := v.buildCreate(req.CreateRequest, pol, next)
	if err != nil {
		return nil, err
	}
	candidate := v.newCandidate(true, next, credentials, bindings)
	candidate.created = created
	return candidate, nil
}

// PrepareDelete creates an authoritative empty tombstone without publishing it.
func (v *Store) PrepareDelete() (*MutationCandidate, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if !v.exists {
		return nil, ErrNotFound
	}
	candidate := v.newCandidate(false, 0, make(map[string]record), make(map[string]Binding))
	candidate.snapshot = &ActiveSnapshot{}
	return candidate, nil
}

func (v *Store) newCandidate(
	exists bool,
	revision int64,
	credentials map[string]record,
	bindings map[string]Binding,
) *MutationCandidate {
	return &MutationCandidate{
		owner:           v,
		baseMutationTag: v.mutationTag,
		exists:          exists,
		revision:        revision,
		credentials:     credentials,
		bindings:        bindings,
	}
}

// Sanitized returns the candidate's public metadata without publishing it.
func (c *MutationCandidate) Sanitized() (State, error) {
	if c == nil {
		return State{}, ErrInvalidCandidate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return State{}, ErrCandidateClosed
	}
	if !c.exists {
		return State{}, ErrNotFound
	}
	return sanitizedState(c.revision, c.credentials, c.bindings), nil
}

// Created reports whether committing this candidate would create the vault
// (only PrepareReplace sets it).
func (c *MutationCandidate) Created() bool {
	return c != nil && c.created
}

// Exists reports whether the candidate describes a live vault revision (a
// delete tombstone does not).
func (c *MutationCandidate) Exists() bool {
	return c != nil && c.exists
}

// Full returns the complete candidate revision INCLUDING credential values —
// exactly what gets encoded into the subject's vault file before commit.
func (c *MutationCandidate) Full() (FullRevision, error) {
	if c == nil {
		return FullRevision{}, ErrInvalidCandidate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return FullRevision{}, ErrCandidateClosed
	}
	if !c.exists {
		return FullRevision{}, ErrNotFound
	}
	full := FullRevision{
		Revision:    c.revision,
		Credentials: make([]Credential, 0, len(c.credentials)),
		Bindings:    make([]Binding, 0, len(c.bindings)),
	}
	for _, r := range c.credentials {
		full.Credentials = append(full.Credentials, Credential{
			Name:   r.Name,
			Source: append(json.RawMessage(nil), r.raw...),
		})
	}
	for _, b := range c.bindings {
		full.Bindings = append(full.Bindings, cloneBinding(b))
	}
	sort.Slice(full.Credentials, func(i, j int) bool { return full.Credentials[i].Name < full.Credentials[j].Name })
	sort.Slice(full.Bindings, func(i, j int) bool { return full.Bindings[i].Name < full.Bindings[j].Name })
	return full, nil
}

// ActiveSnapshot renders the complete candidate for the proxy transaction.
func (c *MutationCandidate) ActiveSnapshot(ctx context.Context) (ActiveSnapshot, error) {
	if c == nil {
		return ActiveSnapshot{}, ErrInvalidCandidate
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ActiveSnapshot{}, ErrCandidateClosed
	}
	if !c.exists {
		return ActiveSnapshot{}, nil
	}
	if c.snapshot != nil {
		return cloneActiveSnapshot(*c.snapshot), nil
	}
	snapshot, err := renderActiveSnapshot(ctx, c.revision, c.credentials, c.bindings)
	if err != nil {
		return ActiveSnapshot{}, err
	}
	c.snapshot = &snapshot
	return cloneActiveSnapshot(snapshot), nil
}

// CommitCandidate publishes exactly one candidate if the Store has not changed
// since it was prepared. The private mutation tag detects public-revision ABA.
// It does not detect policy changes: callers must hold the shared policy/Vault
// mutation barrier from Prepare through Commit when policy affects validation.
func (v *Store) CommitCandidate(candidate *MutationCandidate) (State, error) {
	if candidate == nil {
		return State{}, ErrInvalidCandidate
	}
	candidate.mu.Lock()
	defer candidate.mu.Unlock()
	if candidate.closed {
		return State{}, ErrCandidateClosed
	}
	if candidate.owner != v {
		return State{}, ErrInvalidCandidate
	}
	if candidate.snapshot == nil {
		return State{}, ErrCandidateNotRendered
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if candidate.baseMutationTag != v.mutationTag {
		candidate.closeLocked()
		return State{}, ErrStaleCandidate
	}
	v.publishLocked(candidate.exists, candidate.revision, candidate.credentials, candidate.bindings)
	state := State{}
	if candidate.exists {
		snapshot := cloneActiveSnapshot(*candidate.snapshot)
		v.activeSnapshot = &snapshot
		state = v.sanitizedLocked()
	}
	candidate.closeLocked()
	return state, nil
}

// Discard invalidates a candidate without changing its Store.
func (c *MutationCandidate) Discard() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *MutationCandidate) closeLocked() {
	c.closed = true
	c.owner = nil
	c.credentials = nil
	c.bindings = nil
	c.snapshot = nil
}

func (v *Store) buildCreate(
	req CreateRequest,
	pol *policy.NetworkPolicy,
	revision int64,
) (map[string]record, map[string]Binding, error) {
	credentials := make(map[string]record, len(req.Credentials))
	bindings := make(map[string]Binding, len(req.Bindings))
	for _, credential := range req.Credentials {
		record, err := v.normalizeCredential(credential, revision)
		if err != nil {
			return nil, nil, err
		}
		if _, duplicate := credentials[record.Name]; duplicate {
			return nil, nil, fmt.Errorf("duplicate credential name %q", record.Name)
		}
		credentials[record.Name] = record
	}
	for _, binding := range req.Bindings {
		normalized, err := v.normalizeBinding(binding)
		if err != nil {
			return nil, nil, err
		}
		if _, duplicate := bindings[normalized.Name]; duplicate {
			return nil, nil, fmt.Errorf("duplicate binding name %q", normalized.Name)
		}
		bindings[normalized.Name] = normalized
	}
	if err := v.validateCandidate(credentials, bindings, pol); err != nil {
		return nil, nil, err
	}
	return credentials, bindings, nil
}

func (v *Store) buildPatch(
	req MutationRequest,
	pol *policy.NetworkPolicy,
	nextRevision int64,
) (map[string]record, map[string]Binding, error) {
	credentials := cloneCredentialRecords(v.credentials)
	bindings := cloneCredentialBindings(v.bindings)
	if err := v.applyCredentialMutations(credentials, req.Credentials, nextRevision); err != nil {
		return nil, nil, err
	}
	if err := v.applyBindingMutations(bindings, req.Bindings); err != nil {
		return nil, nil, err
	}
	if err := v.validateCandidate(credentials, bindings, pol); err != nil {
		return nil, nil, err
	}
	return credentials, bindings, nil
}

func (v *Store) publishLocked(
	exists bool,
	revision int64,
	credentials map[string]record,
	bindings map[string]Binding,
) {
	mutationTag := newActiveSnapshotTag()
	v.exists = exists
	v.revision = revision
	v.credentials = credentials
	v.bindings = bindings
	v.mutationTag = mutationTag
	v.activeSnapshot = nil
	if exists {
		v.activeTag = mutationTag
	} else {
		v.activeTag = ""
	}
}

func sanitizedState(revision int64, credentials map[string]record, bindings map[string]Binding) State {
	state := State{
		Revision:    revision,
		Credentials: make([]Metadata, 0, len(credentials)),
		Bindings:    make([]BindingMetadata, 0, len(bindings)),
	}
	for _, credential := range credentials {
		state.Credentials = append(state.Credentials, Metadata{
			Name:       credential.Name,
			SourceType: credential.SourceType,
			Revision:   credential.Revision,
		})
	}
	for _, binding := range bindings {
		state.Bindings = append(state.Bindings, BindingMetadata{
			Name:     binding.Name,
			Revision: revision,
			Match:    cloneMatch(binding.Match),
			Auth:     sanitizeAuth(binding.Auth),
		})
	}
	sort.Slice(state.Credentials, func(i, j int) bool { return state.Credentials[i].Name < state.Credentials[j].Name })
	sort.Slice(state.Bindings, func(i, j int) bool { return state.Bindings[i].Name < state.Bindings[j].Name })
	return state
}

func renderActiveSnapshot(
	ctx context.Context,
	revision int64,
	credentials map[string]record,
	bindings map[string]Binding,
) (ActiveSnapshot, error) {
	snapshot := ActiveSnapshot{
		Revision: revision,
		Bindings: make([]ActiveBinding, 0, len(bindings)),
	}
	redactions := make(map[string]struct{})
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		binding := bindings[name]
		headers, values, err := renderInjectionHeaders(ctx, binding.Auth, credentials)
		if err != nil {
			return ActiveSnapshot{}, err
		}
		substitutions, substitutionValues, err := renderSubstitutions(ctx, binding.Auth, credentials)
		if err != nil {
			return ActiveSnapshot{}, err
		}
		snapshot.Bindings = append(snapshot.Bindings, ActiveBinding{
			Name:          binding.Name,
			Match:         binding.Match,
			Headers:       headers,
			Substitutions: substitutions,
		})
		values = append(values, substitutionValues...)
		for _, value := range values {
			if value != "" {
				redactions[value] = struct{}{}
			}
		}
	}
	for value := range redactions {
		snapshot.Redactions = append(snapshot.Redactions, value)
	}
	sort.Slice(snapshot.Redactions, func(i, j int) bool {
		if len(snapshot.Redactions[i]) != len(snapshot.Redactions[j]) {
			return len(snapshot.Redactions[i]) > len(snapshot.Redactions[j])
		}
		return snapshot.Redactions[i] < snapshot.Redactions[j]
	})
	return snapshot, nil
}

func cloneActiveSnapshot(snapshot ActiveSnapshot) ActiveSnapshot {
	clone := ActiveSnapshot{
		Revision:   snapshot.Revision,
		Bindings:   make([]ActiveBinding, len(snapshot.Bindings)),
		Redactions: append([]string(nil), snapshot.Redactions...),
	}
	for i, binding := range snapshot.Bindings {
		clone.Bindings[i] = ActiveBinding{
			Name:          binding.Name,
			Match:         cloneMatch(binding.Match),
			Headers:       append([]InjectionHeader(nil), binding.Headers...),
			Substitutions: make([]InjectionSubstitution, len(binding.Substitutions)),
		}
		for j, substitution := range binding.Substitutions {
			clone.Bindings[i].Substitutions[j] = InjectionSubstitution{
				Placeholder: substitution.Placeholder,
				Value:       substitution.Value,
				In:          append([]string(nil), substitution.In...),
			}
		}
	}
	return clone
}

func cloneMatch(match Match) Match {
	return Match{
		Schemes: append([]string(nil), match.Schemes...),
		Ports:   append([]int(nil), match.Ports...),
		Hosts:   append([]string(nil), match.Hosts...),
		Methods: append([]string(nil), match.Methods...),
		Paths:   append([]string(nil), match.Paths...),
	}
}

func cloneBinding(binding Binding) Binding {
	return Binding{
		Name:  binding.Name,
		Match: cloneMatch(binding.Match),
		Auth:  cloneAuth(binding.Auth),
	}
}

func cloneAuth(auth Auth) Auth {
	clone := Auth{
		Type:          auth.Type,
		Credential:    auth.Credential,
		Name:          auth.Name,
		Headers:       append([]CustomHeaderEntry(nil), auth.Headers...),
		Substitutions: make([]Substitution, len(auth.Substitutions)),
	}
	for i, substitution := range auth.Substitutions {
		clone.Substitutions[i] = Substitution{
			Credential:  substitution.Credential,
			Placeholder: substitution.Placeholder,
			In:          append([]string(nil), substitution.In...),
		}
	}
	return clone
}
