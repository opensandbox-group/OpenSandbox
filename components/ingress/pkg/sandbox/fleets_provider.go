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

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	fastpathv2 "github.com/alibaba/opensandbox/ingress/pkg/fastpath/v2"
)

const (
	ExecdPort                 = 44772
	EgressPort                = 18080
	FastSandboxCredential     = "X-Fast-Sandbox-Route-Credential"
	FastSandboxProxyError     = "X-Fast-Sandbox-Proxy-Error"
	fastSandboxStaleRoute     = "stale_route"
	fastSandboxCredentialFail = "credential_rejected"
	fastSandboxRouteMissing   = "route_unavailable"
	fastSandboxUpstreamFailed = "upstream_unavailable"
	fastPathConnectTimeout    = 5 * time.Second
	routeCacheSweepInterval   = time.Minute
)

type FastPathResolver interface {
	ResolveEndpoint(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error)
}

type fastPathResolutionError struct {
	public error
	cause  error
}

func (e *fastPathResolutionError) Error() string        { return e.public.Error() }
func (e *fastPathResolutionError) Unwrap() error        { return e.public }
func (e *fastPathResolutionError) InternalCause() error { return e.cause }

type FleetsProvider struct {
	resolver    FastPathResolver
	connection  *grpc.ClientConn
	waitTimeout time.Duration
	accessMode  fastpathv2.EndpointAccessMode
	now         func() time.Time

	mu    sync.RWMutex
	cache map[EndpointTarget]EndpointInfo
}

func NewFleetsProvider(endpoint string, waitTimeout time.Duration, accessMode string) (*FleetsProvider, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("FastPath endpoint is required")
	}
	if waitTimeout > 5*time.Minute {
		return nil, errors.New("FastPath wait timeout cannot exceed five minutes")
	}
	parsedAccessMode, err := parseFastPathAccessMode(accessMode)
	if err != nil {
		return nil, err
	}
	// Phase 1a uses plaintext gRPC inside a NetworkPolicy-isolated cluster.
	// TLS requires matching server support and is intentionally not implied here.
	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("create FastPath client: %w", err)
	}
	provider := NewFleetsProviderWithResolver(fastpathv2.NewFastPathServiceClient(conn), waitTimeout, parsedAccessMode)
	provider.connection = conn
	return provider, nil
}

func NewFleetsProviderWithResolver(resolver FastPathResolver, waitTimeout time.Duration, accessMode fastpathv2.EndpointAccessMode) *FleetsProvider {
	if waitTimeout <= 0 {
		waitTimeout = 2 * time.Second
	}
	return &FleetsProvider{
		resolver:    resolver,
		waitTimeout: waitTimeout,
		accessMode:  accessMode,
		now:         time.Now,
		cache:       make(map[EndpointTarget]EndpointInfo),
	}
}

func parseFastPathAccessMode(value string) (fastpathv2.EndpointAccessMode, error) {
	switch strings.TrimSpace(value) {
	case "central-proxy":
		return fastpathv2.EndpointAccessMode_CENTRAL_PROXY, nil
	case "direct-fastlet-proxy":
		return fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY, nil
	default:
		return 0, fmt.Errorf("unsupported FastPath access mode %q", value)
	}
}

func (p *FleetsProvider) Start(ctx context.Context) error {
	if p.resolver == nil {
		return errors.New("FastPath resolver is required")
	}
	if p.connection != nil {
		connectCtx, cancel := context.WithTimeout(ctx, fastPathConnectTimeout)
		defer cancel()
		if err := waitForFastPathReady(connectCtx, p.connection); err != nil {
			_ = p.connection.Close()
			return fmt.Errorf("connect to FastPath: %w", err)
		}
		go func() {
			<-ctx.Done()
			_ = p.connection.Close()
		}()
	}
	go p.runCacheJanitor(ctx)
	return nil
}

func waitForFastPathReady(ctx context.Context, connection *grpc.ClientConn) error {
	connection.Connect()
	for {
		state := connection.GetState()
		switch state {
		case connectivity.Ready:
			return nil
		case connectivity.Shutdown:
			return errors.New("gRPC channel shut down before becoming ready")
		case connectivity.Idle:
			connection.Connect()
		}
		if !connection.WaitForStateChange(ctx, state) {
			return fmt.Errorf("FastPath channel stuck in state %s: %w", state, ctx.Err())
		}
	}
}

func (p *FleetsProvider) runCacheJanitor(ctx context.Context) {
	ticker := time.NewTicker(routeCacheSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sweepExpired(p.now())
		}
	}
}

func (p *FleetsProvider) sweepExpired(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for target, info := range p.cache {
		if !p.fresh(info, now) {
			delete(p.cache, target)
		}
	}
}

func (*FleetsProvider) RequiresAuthenticatedRouteScope() {}

func (p *FleetsProvider) ResolveEndpoint(ctx context.Context, target EndpointTarget) (*EndpointInfo, error) {
	if target.Namespace == "" || target.SandboxID == "" {
		return nil, errors.New("fleets endpoint target requires namespace and sandbox ID")
	}
	if target.Port < 1 || target.Port > 65535 {
		return nil, fmt.Errorf("invalid target port %d", target.Port)
	}
	if target.Port == EgressPort {
		return nil, fmt.Errorf("%w: egress policy port %d is deferred to Phase 1b", ErrTargetUnsupported, EgressPort)
	}
	if cached, ok := p.cached(target); ok {
		return &cached, nil
	}

	request := &fastpathv2.ResolveEndpointRequest{
		Sandbox: &fastpathv2.SandboxReference{Reference: &fastpathv2.SandboxReference_NamespacedName{
			NamespacedName: &fastpathv2.NamespacedName{Namespace: target.Namespace, Name: target.SandboxID},
		}},
		AccessMode:        p.accessMode,
		WaitUntilReady:    true,
		WaitTimeoutMillis: int32(p.waitTimeout.Milliseconds()),
	}
	if target.Port == ExecdPort {
		request.Target = &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_ComponentName{ComponentName: "execd"}}
	} else {
		request.Target = &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: uint32(target.Port)}}
	}

	rpcCtx, cancel := context.WithTimeout(ctx, p.waitTimeout+5*time.Second)
	defer cancel()
	response, err := p.resolver.ResolveEndpoint(rpcCtx, request)
	if err != nil {
		return nil, mapFastPathError(err)
	}
	info, err := p.endpointInfo(response)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.cache[target] = *info
	p.mu.Unlock()
	return info, nil
}

func (p *FleetsProvider) Invalidate(target EndpointTarget) {
	p.mu.Lock()
	delete(p.cache, target)
	p.mu.Unlock()
}

func (p *FleetsProvider) cached(target EndpointTarget) (EndpointInfo, bool) {
	p.mu.RLock()
	info, ok := p.cache[target]
	p.mu.RUnlock()
	if !ok {
		return EndpointInfo{}, false
	}
	// Refresh before the credential's final five seconds so a stream is not
	// started with a route that is about to expire.
	if !p.fresh(info, p.now()) {
		p.Invalidate(target)
		return EndpointInfo{}, false
	}
	return info, true
}

func (*FleetsProvider) fresh(info EndpointInfo, now time.Time) bool {
	return !info.ExpiresAt.IsZero() && now.Add(5*time.Second).Before(info.ExpiresAt)
}

func (p *FleetsProvider) endpointInfo(response *fastpathv2.ResolveEndpointResponse) (*EndpointInfo, error) {
	if response == nil || response.ProxyEndpoint == "" {
		return nil, errors.New("FastPath returned an empty proxy endpoint")
	}
	parsed, err := url.Parse(response.ProxyEndpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		cause := fmt.Errorf("FastPath returned an invalid proxy endpoint %q without scheme or host", response.ProxyEndpoint)
		if err != nil {
			cause = fmt.Errorf("FastPath returned an invalid proxy endpoint %q: %w", response.ProxyEndpoint, err)
		}
		return nil, &fastPathResolutionError{
			public: errors.New("FastPath returned an invalid proxy endpoint"),
			cause:  cause,
		}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, &fastPathResolutionError{
			public: errors.New("FastPath returned an unsupported proxy scheme"),
			cause:  fmt.Errorf("FastPath returned unsupported proxy scheme %q", parsed.Scheme),
		}
	}
	credential := ""
	seenCredential := false
	for name, value := range response.RequiredHeaders {
		if !strings.EqualFold(name, FastSandboxCredential) {
			return nil, &fastPathResolutionError{
				public: errors.New("FastPath returned unsupported required headers"),
				cause:  fmt.Errorf("FastPath returned unsupported required header %q", name),
			}
		}
		if seenCredential {
			return nil, &fastPathResolutionError{
				public: errors.New("FastPath returned duplicate route credentials"),
				cause:  fmt.Errorf("FastPath returned duplicate %s headers", FastSandboxCredential),
			}
		}
		seenCredential = true
		credential = value
	}
	if credential == "" {
		return nil, &fastPathResolutionError{
			public: errors.New("FastPath returned an invalid route credential"),
			cause:  fmt.Errorf("FastPath response is missing %s", FastSandboxCredential),
		}
	}
	upstreamHeaders := make(http.Header, 1)
	upstreamHeaders.Set(FastSandboxCredential, credential)
	expiresAt := time.Unix(response.ExpiresAtUnixSeconds, 0)
	if response.ExpiresAtUnixSeconds <= 0 || !p.fresh(EndpointInfo{ExpiresAt: expiresAt}, p.now()) {
		return nil, errors.New("FastPath returned an expired or near-expiry route credential")
	}
	return &EndpointInfo{
		UpstreamURL:     parsed.String(),
		UpstreamHeaders: upstreamHeaders,
		ExpiresAt:       expiresAt,
	}, nil
}

func mapFastPathError(err error) error {
	var public error
	switch status.Code(err) {
	case codes.NotFound:
		public = fmt.Errorf("%w: sandbox not found", ErrSandboxNotFound)
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
		public = fmt.Errorf("%w: FastPath resolution temporarily unavailable", ErrSandboxNotReady)
	case codes.Canceled:
		public = errors.New("FastPath resolution canceled")
	default:
		public = fmt.Errorf("FastPath ResolveEndpoint failed: %s", status.Code(err))
	}
	return &fastPathResolutionError{public: public, cause: err}
}

func IsStaleFastPathResponse(header http.Header) bool {
	switch header.Get(FastSandboxProxyError) {
	case fastSandboxStaleRoute, fastSandboxCredentialFail, fastSandboxRouteMissing, fastSandboxUpstreamFailed:
		return true
	default:
		return false
	}
}
