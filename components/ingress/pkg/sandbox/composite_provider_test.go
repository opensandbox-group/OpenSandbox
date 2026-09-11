// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type recordingProvider struct {
	started     bool
	resolved    []EndpointTarget
	invalidated []EndpointTarget
	info        EndpointInfo
}

func (p *recordingProvider) Start(context.Context) error {
	p.started = true
	return nil
}

func (p *recordingProvider) ResolveEndpoint(_ context.Context, target EndpointTarget) (*EndpointInfo, error) {
	p.resolved = append(p.resolved, target)
	info := p.info
	return &info, nil
}

func (p *recordingProvider) Invalidate(target EndpointTarget) {
	p.invalidated = append(p.invalidated, target)
}

func TestCompositeProviderRoutesByExplicitKind(t *testing.T) {
	legacy := &recordingProvider{info: EndpointInfo{Endpoint: "legacy.svc"}}
	fsb := &recordingProvider{info: EndpointInfo{UpstreamURL: "http://fastlet.svc/route"}}
	provider := NewCompositeProvider(legacy, fsb)

	require.NoError(t, provider.Start(context.Background()))
	require.True(t, legacy.started)
	require.True(t, fsb.started)

	legacyTarget := EndpointTarget{SandboxID: "legacy-sandbox", Port: 8080}
	info, err := provider.ResolveEndpoint(context.Background(), legacyTarget)
	require.NoError(t, err)
	require.Equal(t, "legacy.svc", info.Endpoint)
	require.Equal(t, []EndpointTarget{legacyTarget}, legacy.resolved)
	require.Empty(t, fsb.resolved)

	fsbTarget := EndpointTarget{RouteKind: RouteKindFastSandbox, Namespace: "tenant-a", SandboxID: "fsb-sandbox", Port: ExecdPort}
	info, err = provider.ResolveEndpoint(context.Background(), fsbTarget)
	require.NoError(t, err)
	require.Equal(t, "http://fastlet.svc/route", info.UpstreamURL)
	require.Equal(t, []EndpointTarget{fsbTarget}, fsb.resolved)
}

func TestCompositeProviderInvalidatesOnlyFastSandboxRoutes(t *testing.T) {
	legacy := &recordingProvider{}
	fsb := &recordingProvider{}
	provider := NewCompositeProvider(legacy, fsb)

	provider.Invalidate(EndpointTarget{SandboxID: "legacy-sandbox", Port: 8080})
	require.Empty(t, legacy.invalidated)
	require.Empty(t, fsb.invalidated)

	fsbTarget := EndpointTarget{RouteKind: RouteKindFastSandbox, Namespace: "tenant-a", SandboxID: "fsb-sandbox", Port: ExecdPort}
	provider.Invalidate(fsbTarget)
	require.Equal(t, []EndpointTarget{fsbTarget}, fsb.invalidated)
}

func TestCompositeProviderRejectsUnknownRouteKind(t *testing.T) {
	provider := NewCompositeProvider(&recordingProvider{}, &recordingProvider{})

	_, err := provider.ResolveEndpoint(context.Background(), EndpointTarget{RouteKind: RouteKind(2), SandboxID: "sandbox", Port: 8080})
	require.ErrorIs(t, err, ErrTargetUnsupported)
	require.EqualError(t, err, "sandbox target not supported: route kind 2")
}
