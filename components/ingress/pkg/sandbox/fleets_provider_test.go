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
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	fastpathv2 "github.com/alibaba/opensandbox/ingress/pkg/fastpath/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type fakeFastPathResolver struct {
	requests []*fastpathv2.ResolveEndpointRequest
	now      time.Time
}

func (f *fakeFastPathResolver) ResolveEndpoint(_ context.Context, request *fastpathv2.ResolveEndpointRequest, _ ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
	f.requests = append(f.requests, request)
	namespace := request.GetSandbox().GetNamespacedName().GetNamespace()
	return &fastpathv2.ResolveEndpointResponse{
		ProxyEndpoint:        "http://sandbox-proxy:8080/v2/" + namespace,
		RequiredHeaders:      map[string]string{FastSandboxCredential: "credential-" + namespace},
		ExpiresAtUnixSeconds: f.now.Add(time.Minute).Unix(),
	}, nil
}

func TestFleetsProviderMapsTargetsAndCachesByNamespace(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	resolver := &fakeFastPathResolver{now: now}
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
	provider.now = func() time.Time { return now }

	for _, namespace := range []string{"tenant-a", "tenant-b"} {
		target := EndpointTarget{Namespace: namespace, SandboxID: "same-id", Port: ExecdPort}
		info, err := provider.ResolveEndpoint(context.Background(), target)
		require.NoError(t, err)
		require.Equal(t, "credential-"+namespace, info.UpstreamHeaders.Get(FastSandboxCredential))
		_, err = provider.ResolveEndpoint(context.Background(), target)
		require.NoError(t, err)
	}

	require.Len(t, resolver.requests, 2)
	for _, request := range resolver.requests {
		require.Equal(t, "execd", request.GetTarget().GetComponentName())
		require.Equal(t, fastpathv2.EndpointAccessMode_CENTRAL_PROXY, request.GetAccessMode())
		require.True(t, request.GetWaitUntilReady())
	}
}

func TestFleetsProviderUsesRawPortAndRefreshesNearExpiry(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	resolver := &fakeFastPathResolver{now: now}
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY)
	provider.now = func() time.Time { return now }
	target := EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080}

	_, err := provider.ResolveEndpoint(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, uint32(8080), resolver.requests[0].GetTarget().GetPort())
	require.Equal(t, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY, resolver.requests[0].GetAccessMode())

	now = now.Add(56 * time.Second)
	resolver.now = now
	_, err = provider.ResolveEndpoint(context.Background(), target)
	require.NoError(t, err)
	require.Len(t, resolver.requests, 2)
}

func TestFleetsProviderSweepRemovesExpiredEntries(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	resolver := &fakeFastPathResolver{now: now}
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY)
	provider.now = func() time.Time { return now }
	oldTarget := EndpointTarget{Namespace: "tenant-a", SandboxID: "old", Port: 8080}
	newTarget := EndpointTarget{Namespace: "tenant-a", SandboxID: "new", Port: 8080}

	_, err := provider.ResolveEndpoint(context.Background(), oldTarget)
	require.NoError(t, err)
	now = now.Add(56 * time.Second)
	resolver.now = now
	_, err = provider.ResolveEndpoint(context.Background(), newTarget)
	require.NoError(t, err)
	provider.sweepExpired(now)

	require.NotContains(t, provider.cache, oldTarget)
	require.Contains(t, provider.cache, newTarget)
}

func TestWaitForFastPathReady(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, waitForFastPathReady(ctx, connection))
}

func TestWaitForFastPathReadyTimesOut(t *testing.T) {
	connection, err := grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = connection.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, waitForFastPathReady(ctx, connection), context.DeadlineExceeded)
}

func TestFleetsProviderRejectsPhase1aEgressWithoutRPC(t *testing.T) {
	resolver := &fakeFastPathResolver{now: time.Now()}
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
	_, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: EgressPort})
	require.ErrorIs(t, err, ErrTargetUnsupported)
	require.Empty(t, resolver.requests)
}

func TestFleetsProviderRejectsMissingCredential(t *testing.T) {
	resolver := fastPathResolverFunc(func(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
		return &fastpathv2.ResolveEndpointResponse{
			ProxyEndpoint:        "http://sandbox-proxy:8080/v2/sandbox",
			ExpiresAtUnixSeconds: time.Now().Add(time.Minute).Unix(),
		}, nil
	})
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
	_, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080})
	require.EqualError(t, err, "FastPath returned an invalid route credential")
	detailed := err.(interface{ InternalCause() error })
	require.EqualError(t, detailed.InternalCause(), fmt.Sprintf("FastPath response is missing %s", FastSandboxCredential))
}

func TestFleetsProviderAcceptsCaseInsensitiveCredentialHeader(t *testing.T) {
	resolver := fastPathResolverFunc(func(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
		return &fastpathv2.ResolveEndpointResponse{
			ProxyEndpoint:        "http://fastlet-proxy:5780/v2/sandboxes/uid/components/execd",
			RequiredHeaders:      map[string]string{"x-fast-sandbox-route-credential": "credential"},
			ExpiresAtUnixSeconds: time.Now().Add(time.Minute).Unix(),
		}, nil
	})
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY)
	info, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: ExecdPort})
	require.NoError(t, err)
	require.Equal(t, "credential", info.UpstreamHeaders.Get(FastSandboxCredential))
}

func TestFleetsProviderRejectsDuplicateCredentialHeaders(t *testing.T) {
	resolver := fastPathResolverFunc(func(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
		return &fastpathv2.ResolveEndpointResponse{
			ProxyEndpoint: "http://fastlet-proxy:5780/v2/sandboxes/uid/components/execd",
			RequiredHeaders: map[string]string{
				FastSandboxCredential:             "credential",
				"x-fast-sandbox-route-credential": "",
			},
			ExpiresAtUnixSeconds: time.Now().Add(time.Minute).Unix(),
		}, nil
	})
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY)
	_, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: ExecdPort})
	require.EqualError(t, err, "FastPath returned duplicate route credentials")
}

func TestFleetsProviderRejectsUnsupportedRequiredHeaders(t *testing.T) {
	for _, header := range []string{"Host", "Authorization"} {
		t.Run(header, func(t *testing.T) {
			resolver := fastPathResolverFunc(func(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
				return &fastpathv2.ResolveEndpointResponse{
					ProxyEndpoint: "http://fastlet-proxy:5780/v2/sandboxes/uid/components/execd",
					RequiredHeaders: map[string]string{
						FastSandboxCredential: "credential",
						header:                "unexpected",
					},
					ExpiresAtUnixSeconds: time.Now().Add(time.Minute).Unix(),
				}, nil
			})
			provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY)
			_, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: ExecdPort})
			require.EqualError(t, err, "FastPath returned unsupported required headers")
			detailed := err.(interface{ InternalCause() error })
			require.Contains(t, detailed.InternalCause().Error(), header)
		})
	}
}

func TestFleetsProviderRejectsNearExpiryCredential(t *testing.T) {
	resolver := fastPathResolverFunc(func(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
		return &fastpathv2.ResolveEndpointResponse{
			ProxyEndpoint:        "http://fastlet-proxy:5780/v2/sandboxes/uid/components/execd",
			RequiredHeaders:      map[string]string{FastSandboxCredential: "credential"},
			ExpiresAtUnixSeconds: time.Now().Add(3 * time.Second).Unix(),
		}, nil
	})
	provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY)
	_, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: ExecdPort})
	require.EqualError(t, err, "FastPath returned an expired or near-expiry route credential")
}

func TestMapFastPathErrorDoesNotExposeInternalDetails(t *testing.T) {
	internalDetail := "dial tcp 10.0.3.17:9090: connect: connection refused"
	err := mapFastPathError(status.Error(codes.Unavailable, internalDetail))
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.NotContains(t, err.Error(), internalDetail)
	detailed, ok := err.(interface{ InternalCause() error })
	require.True(t, ok)
	require.Contains(t, detailed.InternalCause().Error(), internalDetail)

	err = mapFastPathError(status.Error(codes.Internal, internalDetail))
	require.False(t, strings.Contains(err.Error(), internalDetail))
}

func TestFleetsProviderDoesNotExposeInvalidProxyEndpoint(t *testing.T) {
	for _, internalEndpoint := range []string{"http://10.0.3.17:5780/%zz", "/missing-authority"} {
		t.Run(internalEndpoint, func(t *testing.T) {
			resolver := fastPathResolverFunc(func(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
				return &fastpathv2.ResolveEndpointResponse{
					ProxyEndpoint:        internalEndpoint,
					RequiredHeaders:      map[string]string{FastSandboxCredential: "credential"},
					ExpiresAtUnixSeconds: time.Now().Add(time.Minute).Unix(),
				}, nil
			})
			provider := NewFleetsProviderWithResolver(resolver, time.Second, fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY)
			_, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: ExecdPort})
			require.EqualError(t, err, "FastPath returned an invalid proxy endpoint")
			detailed := err.(interface{ InternalCause() error })
			require.Contains(t, detailed.InternalCause().Error(), internalEndpoint)
			require.NotContains(t, detailed.InternalCause().Error(), "%!w")
		})
	}
}

func TestNewFleetsProviderRejectsUnknownAccessMode(t *testing.T) {
	_, err := NewFleetsProvider("fastpath:9090", time.Second, "unknown")
	require.EqualError(t, err, `unsupported FastPath access mode "unknown"`)
}

func TestParseFastPathAccessMode(t *testing.T) {
	for value, expected := range map[string]fastpathv2.EndpointAccessMode{
		"central-proxy":        fastpathv2.EndpointAccessMode_CENTRAL_PROXY,
		"direct-fastlet-proxy": fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY,
	} {
		actual, err := parseFastPathAccessMode(value)
		require.NoError(t, err)
		require.Equal(t, expected, actual)
	}
}

type fastPathResolverFunc func(context.Context, *fastpathv2.ResolveEndpointRequest, ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error)

func (f fastPathResolverFunc) ResolveEndpoint(ctx context.Context, request *fastpathv2.ResolveEndpointRequest, options ...grpc.CallOption) (*fastpathv2.ResolveEndpointResponse, error) {
	return f(ctx, request, options...)
}
