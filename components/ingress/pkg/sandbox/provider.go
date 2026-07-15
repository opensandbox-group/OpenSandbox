// Copyright 2025 Alibaba Group Holding Ltd.
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

package sandbox

import (
	"context"
	"errors"
)

type ProviderType string

const (
	ProviderTypeBatchSandbox ProviderType = "batchsandbox"
	ProviderTypeAgentSandbox ProviderType = "agent-sandbox"

	sandboxNameIndex string = "sandbox-name"

	// AnnotationAccessToken marks a sandbox that requires signed ingress routes when non-empty.
	AnnotationAccessToken = "opensandbox.io/secure-access-token"
)

func (tpy ProviderType) String() string { return string(tpy) }

var (
	// ErrSandboxNotFound indicates the sandbox resource does not exist
	ErrSandboxNotFound = errors.New("sandbox not found")

	// ErrSandboxNotReady indicates the sandbox exists but is not ready
	// This includes: not enough ready replicas, missing endpoints, invalid configuration
	ErrSandboxNotReady = errors.New("sandbox not ready")
)

// Provider defines the interface for sandbox resource providers
// Implementations include BatchSandboxProvider, AgentSandboxProvider, etc.
type Provider interface {
	// GetEndpoint retrieves endpoint and secure-access metadata for a sandbox by its id/name.
	// Providers run in global-watch mode across all namespaces.
	// Returns the first available endpoint from provider status/annotations.
	// Returns error if sandbox not found or endpoint unavailable.
	// Note: This is a local cache query, no network I/O involved
	GetEndpoint(sandboxId string) (*EndpointInfo, error)

	// Start initializes and starts the provider's informer cache
	// Waits for cache sync before returning
	// Must be called before using GetEndpoint
	Start(ctx context.Context) error
}

// ProviderFactory creates a Provider instance based on the provider type
type ProviderFactory interface {
	CreateProvider(providerType ProviderType) (Provider, error)
}
