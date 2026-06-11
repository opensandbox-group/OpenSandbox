// Copyright 2026 Alibaba Group Holding Ltd.
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

package opensandbox

import "context"

// GetEgressPolicy retrieves the current egress network policy.
func (s *Sandbox) GetEgressPolicy(ctx context.Context) (*PolicyStatusResponse, error) {
	if err := s.resolveEgress(ctx); err != nil {
		return nil, err
	}
	return s.egress.GetPolicy(ctx)
}

// PatchEgressRules merges network rules into the current egress policy.
func (s *Sandbox) PatchEgressRules(ctx context.Context, rules []NetworkRule) (*PolicyStatusResponse, error) {
	if err := s.resolveEgress(ctx); err != nil {
		return nil, err
	}
	return s.egress.PatchPolicy(ctx, rules)
}

// DeleteEgressRules removes egress rules matching the given targets from the
// current egress policy. Targets not present in the policy are silently ignored.
func (s *Sandbox) DeleteEgressRules(ctx context.Context, targets []string) (*PolicyStatusResponse, error) {
	if err := s.resolveEgress(ctx); err != nil {
		return nil, err
	}
	return s.egress.DeletePolicy(ctx, targets)
}

// CredentialVault returns the sandbox-scoped egress client used for Credential
// Vault operations.
func (s *Sandbox) CredentialVault(ctx context.Context) (*EgressClient, error) {
	if err := s.resolveEgress(ctx); err != nil {
		return nil, err
	}
	return s.egress, nil
}

// CreateCredentialVault creates the initial sandbox-local Credential Vault.
func (s *Sandbox) CreateCredentialVault(ctx context.Context, req CredentialVaultCreateRequest) (*CredentialVaultState, error) {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return nil, err
	}
	return client.CreateCredentialVault(ctx, req)
}

// GetCredentialVault returns sanitized Credential Vault state.
func (s *Sandbox) GetCredentialVault(ctx context.Context) (*CredentialVaultState, error) {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetCredentialVault(ctx)
}

// PatchCredentialVault atomically mutates sandbox-local credentials and
// bindings.
func (s *Sandbox) PatchCredentialVault(ctx context.Context, req CredentialVaultPatchRequest) (*CredentialVaultState, error) {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return nil, err
	}
	return client.PatchCredentialVault(ctx, req)
}

// DeleteCredentialVault deletes the sandbox-local Credential Vault.
func (s *Sandbox) DeleteCredentialVault(ctx context.Context) error {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return err
	}
	return client.DeleteCredentialVault(ctx)
}

// ListCredentialVaultCredentials returns sanitized credential metadata.
func (s *Sandbox) ListCredentialVaultCredentials(ctx context.Context) (*CredentialListResponse, error) {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListCredentialVaultCredentials(ctx)
}

// GetCredentialVaultCredential returns sanitized metadata for one credential.
func (s *Sandbox) GetCredentialVaultCredential(ctx context.Context, name string) (*CredentialMetadata, error) {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetCredentialVaultCredential(ctx, name)
}

// ListCredentialVaultBindings returns sanitized binding metadata.
func (s *Sandbox) ListCredentialVaultBindings(ctx context.Context) (*CredentialBindingListResponse, error) {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return nil, err
	}
	return client.ListCredentialVaultBindings(ctx)
}

// GetCredentialVaultBinding returns sanitized metadata for one binding.
func (s *Sandbox) GetCredentialVaultBinding(ctx context.Context, name string) (*CredentialBindingMetadata, error) {
	client, err := s.CredentialVault(ctx)
	if err != nil {
		return nil, err
	}
	return client.GetCredentialVaultBinding(ctx, name)
}
