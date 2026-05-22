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
