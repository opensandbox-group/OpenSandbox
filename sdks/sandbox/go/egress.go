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

import (
	"context"
	"net/url"
)

// EgressClient provides methods for the OpenSandbox Egress API.
// It connects to the egress sidecar endpoint running inside a specific sandbox.
type EgressClient struct {
	*Client
}

// egressAuthHeader is the authentication header used by the Egress sidecar API.
const egressAuthHeader = "OPENSANDBOX-EGRESS-AUTH"

// NewEgressClient creates a new EgressClient.
// baseURL is the sandbox-specific egress sidecar endpoint
// (e.g. "http://localhost:18080").
// authToken is the value for the OPENSANDBOX-EGRESS-AUTH header; pass ""
// if the sidecar does not require authentication.
func NewEgressClient(baseURL, authToken string, opts ...Option) *EgressClient {
	return &EgressClient{
		Client: NewClient(baseURL, authToken, egressAuthHeader, opts...),
	}
}

// GetPolicy returns the currently enforced egress policy and sidecar metadata.
func (c *EgressClient) GetPolicy(ctx context.Context) (*PolicyStatusResponse, error) {
	var resp PolicyStatusResponse
	if err := c.doRequest(ctx, "GET", "/policy", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PatchPolicy merges the given network rules into the current egress policy.
// Existing rules remain unless overridden. Rule conflict behavior is determined
// by the server.
func (c *EgressClient) PatchPolicy(ctx context.Context, rules []NetworkRule) (*PolicyStatusResponse, error) {
	var resp PolicyStatusResponse
	if err := c.doRequest(ctx, "PATCH", "/policy", rules, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeletePolicy removes egress rules matching the given targets from the current
// policy. Each target is a FQDN or wildcard domain. Targets not present in the
// policy are silently ignored (idempotent). The current defaultAction is
// preserved.
func (c *EgressClient) DeletePolicy(ctx context.Context, targets []string) (*PolicyStatusResponse, error) {
	var resp PolicyStatusResponse
	if err := c.doRequest(ctx, "DELETE", "/policy", targets, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CreateCredentialVault creates the initial sandbox-local Credential Vault
// revision and activates it in Credential Proxy.
func (c *EgressClient) CreateCredentialVault(ctx context.Context, req CredentialVaultCreateRequest) (*CredentialVaultState, error) {
	var resp CredentialVaultState
	if err := c.doRequest(ctx, "POST", "/credential-vault", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetCredentialVault returns sanitized Credential Vault state. Plaintext
// credential values are never part of the returned model.
func (c *EgressClient) GetCredentialVault(ctx context.Context) (*CredentialVaultState, error) {
	var resp CredentialVaultState
	if err := c.doRequest(ctx, "GET", "/credential-vault", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PatchCredentialVault atomically mutates sandbox-local credentials and
// bindings.
func (c *EgressClient) PatchCredentialVault(ctx context.Context, req CredentialVaultPatchRequest) (*CredentialVaultState, error) {
	var resp CredentialVaultState
	if err := c.doRequest(ctx, "PATCH", "/credential-vault", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteCredentialVault deletes the sandbox-local Credential Vault.
func (c *EgressClient) DeleteCredentialVault(ctx context.Context) error {
	return c.doRequest(ctx, "DELETE", "/credential-vault", nil, nil)
}

// ListCredentialVaultCredentials returns sanitized credential metadata.
func (c *EgressClient) ListCredentialVaultCredentials(ctx context.Context) (*CredentialListResponse, error) {
	var resp CredentialListResponse
	if err := c.doRequest(ctx, "GET", "/credential-vault/credentials", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetCredentialVaultCredential returns sanitized metadata for one credential.
func (c *EgressClient) GetCredentialVaultCredential(ctx context.Context, name string) (*CredentialMetadata, error) {
	var resp CredentialMetadata
	if err := c.doRequest(ctx, "GET", "/credential-vault/credentials/"+url.PathEscape(name), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListCredentialVaultBindings returns sanitized binding metadata.
func (c *EgressClient) ListCredentialVaultBindings(ctx context.Context) (*CredentialBindingListResponse, error) {
	var resp CredentialBindingListResponse
	if err := c.doRequest(ctx, "GET", "/credential-vault/bindings", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetCredentialVaultBinding returns sanitized metadata for one binding.
func (c *EgressClient) GetCredentialVaultBinding(ctx context.Context, name string) (*CredentialBindingMetadata, error) {
	var resp CredentialBindingMetadata
	if err := c.doRequest(ctx, "GET", "/credential-vault/bindings/"+url.PathEscape(name), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
