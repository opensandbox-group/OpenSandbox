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
	ProviderTypeFleets       ProviderType = "fleets"

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

	// ErrTargetUnsupported indicates that a route handle exists but traffic to
	// the requested target is not implemented by this provider.
	ErrTargetUnsupported = errors.New("sandbox target not supported")
)

// RouteKind identifies the backend selected by the verified ingress route format.
type RouteKind uint8

const (
	// RouteKindLegacy is the zero value so existing target construction keeps
	// selecting the configured Kubernetes provider.
	RouteKindLegacy RouteKind = iota
	// RouteKindFleets selects FastPath-backed endpoint resolution.
	RouteKindFleets
)

type EndpointTarget struct {
	RouteKind RouteKind
	Namespace string
	SandboxID string
	Port      int
}

// Provider defines the interface for sandbox resource providers
// Implementations include BatchSandboxProvider, AgentSandboxProvider, etc.
type Provider interface {
	// ResolveEndpoint retrieves the complete upstream route for one target.
	// Kubernetes providers answer from their informer cache. The fleets provider
	// may perform a bounded FastPath RPC and cache the resulting short-lived route.
	ResolveEndpoint(ctx context.Context, target EndpointTarget) (*EndpointInfo, error)

	// Start initializes and starts the provider's informer cache
	// Waits for cache sync before returning
	// Must be called before using ResolveEndpoint.
	Start(ctx context.Context) error
}

// EndpointInvalidator is implemented by providers with expiring or
// assignment-fenced routes. The proxy calls it after an authenticated upstream
// reports that a cached route is stale; the current request is not replayed.
type EndpointInvalidator interface {
	Invalidate(target EndpointTarget)
}

// AuthenticatedRouteProvider marks providers that reject legacy unsigned
// ingress routes and require a verified tenant-scoped handle.
type AuthenticatedRouteProvider interface {
	RequiresAuthenticatedRouteScope()
}

// ProviderFactory creates a Provider instance based on the provider type
type ProviderFactory interface {
	CreateProvider(providerType ProviderType) (Provider, error)
}
