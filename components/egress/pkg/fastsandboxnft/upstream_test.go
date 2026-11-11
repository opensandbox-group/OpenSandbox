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

package fastsandboxnft

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/stretchr/testify/require"
)

func TestUpstreamProxyLearnDuringRefreshSurvivesRebuild(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx := context.Background()
	old := []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.0.0.1")}}
	learned := []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.0.0.2")}}
	require.NoError(t, a.ApplyReset(ctx))
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, old))
	started, finish := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- a.refreshUpstreamProxyIPs(ctx, "proxy.test", func(context.Context, string) ([]nftables.ResolvedIP, error) {
			close(started)
			<-finish
			return old, nil
		})
	}()
	<-started
	err := a.AddUpstreamProxyIPs(ctx, learned)
	close(finish)
	require.NoError(t, err)
	require.NoError(t, <-result)
	require.NoError(t, a.ApplyReset(ctx))
	require.Contains(t, runner.last(), "{ 10.0.0.2 }", "an answer learned during lookup must survive its stale snapshot")

	// A new DNS observation resets the absence count.
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, learned))
	require.NoError(t, a.SyncUpstreamProxyIPs(ctx, old))
	require.Contains(t, a.upstreamIPs, learned[0].Addr)
	require.NoError(t, a.SyncUpstreamProxyIPs(ctx, old))
	require.NotContains(t, a.upstreamIPs, learned[0].Addr)
}

func TestUpstreamProxyFailedSyncDoesNotAdvanceAbsence(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx := context.Background()
	old := netip.MustParseAddr("10.0.0.1")
	next := []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.0.0.2")}}
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: old}}))
	runner.fail = func(string) error { return fmt.Errorf("nft unavailable") }
	require.Error(t, a.SyncUpstreamProxyIPs(ctx, next))
	require.Equal(t, uint8(0), a.upstreamIPs[old])
	runner.fail = nil
	require.NoError(t, a.SyncUpstreamProxyIPs(ctx, next))
	require.Contains(t, a.upstreamIPs, old)
	require.NoError(t, a.SyncUpstreamProxyIPs(ctx, next))
	require.NotContains(t, a.upstreamIPs, old)
}

func TestUpstreamProxyPartialRefreshAddsWithoutPruning(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx := context.Background()
	old := netip.MustParseAddr("10.0.0.1")
	current := []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.0.0.2")}}
	newIP := netip.MustParseAddr("10.0.0.3")
	require.NoError(t, a.AddUpstreamProxyIPs(ctx, []nftables.ResolvedIP{{Addr: old}}))
	require.NoError(t, a.SyncUpstreamProxyIPs(ctx, current))
	require.Equal(t, uint8(1), a.upstreamIPs[old])
	err := a.refreshUpstreamProxyIPs(ctx, "proxy.test", func(context.Context, string) ([]nftables.ResolvedIP, error) {
		return []nftables.ResolvedIP{{Addr: newIP}}, fmt.Errorf("pod resolver unavailable")
	})
	require.ErrorContains(t, err, "pod resolver unavailable")
	require.Contains(t, a.upstreamIPs, old)
	require.Contains(t, a.upstreamIPs, current[0].Addr)
	require.Contains(t, a.upstreamIPs, newIP)
	require.NoError(t, a.ApplyReset(ctx))
	require.Contains(t, runner.last(), "{ 10.0.0.1 }")
	require.Contains(t, runner.last(), "{ 10.0.0.3 }")
}

func TestUpstreamProxyRefreshUsesSeparateApplyContext(t *testing.T) {
	var lookupCtx context.Context
	applied := false
	a := NewApplier(func(ctx context.Context, _ string) ([]byte, error) {
		require.ErrorIs(t, lookupCtx.Err(), context.Canceled)
		require.NoError(t, ctx.Err(), "nft must not inherit the completed lookup's cancellation")
		applied = true
		return nil, nil
	}, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	require.NoError(t, a.refreshUpstreamProxyIPs(context.Background(), "proxy.test", func(ctx context.Context, _ string) ([]nftables.ResolvedIP, error) {
		lookupCtx = ctx
		return []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.0.0.1")}}, nil
	}))
	require.True(t, applied)
}

func TestUpstreamProxySeedFailurePreservesPreviousTable(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			runner := &fakeRunner{}
			opts := Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}}
			previous := NewApplier(runner.Run, opts)
			require.NoError(t, previous.AddUpstreamProxyIPs(context.Background(), []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.0.0.1")}}))
			require.NoError(t, previous.ApplyReset(context.Background()))
			before := runner.count()
			restarted := NewApplier(runner.Run, opts)
			restarted.upstreamSeedTimeout = 30 * time.Millisecond
			err := restarted.SeedUpstreamProxyIPs(context.Background(), "proxy.test", func(ctx context.Context, _ string) ([]nftables.ResolvedIP, error) {
				if partial {
					return []nftables.ResolvedIP{{Addr: netip.MustParseAddr("10.0.0.2")}}, fmt.Errorf("one authority failed")
				}
				<-ctx.Done()
				return nil, ctx.Err()
			})
			require.Error(t, err)
			require.Equal(t, before, runner.count(), "failed startup must not reset or update the previous table")
			require.Contains(t, runner.last(), "{ 10.0.0.1 }")
			require.Empty(t, restarted.upstreamIPs)
		})
	}
}

func TestUpstreamProxyConcurrentLearningSyncAndReset(t *testing.T) {
	runner := &fakeRunner{}
	a := NewApplier(runner.Run, Options{UpstreamProxy: &UpstreamProxyEndpoint{Port: 3128}})
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for worker := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				ips := []nftables.ResolvedIP{{Addr: netip.AddrFrom4([4]byte{10, 0, byte(worker), byte(i + 1)})}}
				var err error
				switch worker {
				case 0:
					err = a.AddUpstreamProxyIPs(ctx, ips)
				case 1:
					err = a.SyncUpstreamProxyIPs(ctx, ips)
				case 2:
					err = a.ApplyReset(ctx)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.NoError(t, a.ApplyReset(ctx))
	for addr := range a.upstreamIPs {
		require.Contains(t, runner.last(), fmt.Sprintf("{ %s }", addr))
	}
}
