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

package nftables

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConnectionTrackerSetDynamicIPs(t *testing.T) {
	now := time.Unix(1_000, 0)
	tracker := newConnectionTracker()
	tracker.now = func() time.Time { return now }
	tracker.setDynamicIPs([]ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: 10 * time.Second},
		{Addr: netip.MustParseAddr("::ffff:192.0.2.1"), TTL: 120 * time.Second},
	})

	require.Equal(t, map[netip.Addr]time.Time{
		netip.MustParseAddr("1.1.1.1"):   now.Add(70 * time.Second),
		netip.MustParseAddr("192.0.2.1"): now.Add(180 * time.Second),
	}, tracker.dynamicIPs)
}

func TestConnectionTrackerRefreshCandidates(t *testing.T) {
	now := time.Unix(1_000, 0)
	tracker := newConnectionTracker()
	tracker.now = func() time.Time { return now }
	tracker.setDynamicIPs([]ResolvedIP{
		{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute},
		{Addr: netip.MustParseAddr("2001:db8::1"), TTL: time.Minute},
	})

	refresh := tracker.refreshCandidates([]tcpConnection{
		{remote: netip.MustParseAddr("2001:db8::1"), state: "ESTABLISHED"},
		{remote: netip.MustParseAddr("1.1.1.1"), state: "SYN_SENT"},
		{remote: netip.MustParseAddr("2.2.2.2"), state: "ESTABLISHED"},
		{remote: netip.MustParseAddr("1.1.1.1"), state: "TIME_WAIT"},
	})

	require.Equal(t, []netip.Addr{
		netip.MustParseAddr("1.1.1.1"),
		netip.MustParseAddr("2001:db8::1"),
	}, refresh.addresses)
	require.Equal(t, map[netip.Addr]struct{}{
		netip.MustParseAddr("1.1.1.1"):     {},
		netip.MustParseAddr("2001:db8::1"): {},
	}, refresh.active)
}

func TestConnectionTrackerFinalRefreshAfterClose(t *testing.T) {
	now := time.Unix(1_000, 0)
	addr := netip.MustParseAddr("1.1.1.1")
	tracker := newConnectionTracker()
	tracker.now = func() time.Time { return now }
	tracker.setDynamicIPs([]ResolvedIP{{Addr: addr, TTL: time.Minute}})
	tracker.recordRefresh(tracker.refreshCandidates([]tcpConnection{{remote: addr, state: "ESTABLISHED"}}))

	refresh := tracker.refreshCandidates(nil)
	require.Equal(t, []netip.Addr{addr}, refresh.addresses)
	tracker.recordRefresh(refresh)
	require.Empty(t, tracker.refreshCandidates(nil).addresses)
	require.Equal(t, now.Add(6*time.Minute), tracker.dynamicIPs[addr])
}

func TestConnectionTrackerForgetsExpiredInactiveIP(t *testing.T) {
	now := time.Unix(1_000, 0)
	tracker := newConnectionTracker()
	tracker.now = func() time.Time { return now }
	tracker.setDynamicIPs([]ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: 10 * time.Second}})
	now = now.Add(71 * time.Second)

	require.Empty(t, tracker.refreshCandidates(nil).addresses)
	require.Empty(t, tracker.dynamicIPs)
}

func TestConnectionTrackerClear(t *testing.T) {
	tracker := newConnectionTracker()
	tracker.setDynamicIPs([]ResolvedIP{{Addr: netip.MustParseAddr("1.1.1.1"), TTL: time.Minute}})
	tracker.previousActiveIPs[netip.MustParseAddr("1.1.1.1")] = struct{}{}

	tracker.clear()
	require.Empty(t, tracker.dynamicIPs)
	require.Empty(t, tracker.previousActiveIPs)
}
