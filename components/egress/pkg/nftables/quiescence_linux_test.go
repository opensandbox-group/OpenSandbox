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

//go:build linux

package nftables

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNftQuiescenceAfterCommittedStaticError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !isolatedNftTest(t, ctx) {
		return
	}

	oldPolicy, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"*.example.com"},{"action":"allow","target":"198.51.100.10"}]}`)
	require.NoError(t, err)
	newPolicy, err := policy.ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"198.51.100.20"}]}`)
	require.NoError(t, err)
	committedError := errors.New("injected result loss after successful nft commit")
	var calls atomic.Int32
	var injectCommittedError atomic.Bool
	m := NewManagerWithRunnerAndOptions(func(ctx context.Context, script string) ([]byte, error) {
		calls.Add(1)
		output, err := defaultRunner(ctx, script)
		if err != nil {
			return output, err
		}
		// Injection follows the real successful nft batch, never a simulated
		// submission or an unsuccessful setup command.
		if injectCommittedError.Swap(false) {
			return output, committedError
		}
		return output, nil
	}, Options{
		QuiesceOnApplyFailure: true,
		UpstreamProxy:         &UpstreamProxyEndpoint{Port: 3128, UID: 10042},
	})
	require.NoError(t, m.ApplyStatic(ctx, oldPolicy))
	addresses := func(v4, v6 string) []ResolvedIP {
		return []ResolvedIP{{Addr: netip.MustParseAddr(v4)}, {Addr: netip.MustParseAddr(v6)}}
	}
	directIPs := addresses("192.0.2.10", "2001:db8::10")
	domainIPs := addresses("192.0.2.20", "2001:db8::20")
	tcpIPs := addresses("192.0.2.30", "2001:db8::30")
	upstreamIPs := addresses("192.0.2.40", "2001:db8::40")
	connections := []tcpConnection{
		{remote: tcpIPs[0].Addr, state: "ESTABLISHED"},
		{remote: tcpIPs[1].Addr, state: "ESTABLISHED"},
	}
	require.NoError(t, m.AddResolvedIPs(ctx, directIPs))
	require.NoError(t, m.AddResolvedDomain(ctx, "old.example.com", domainIPs))
	require.NoError(t, m.AddResolvedIPs(ctx, tcpIPs))
	// Feed observed TCP addresses through the production renewal path. No
	// external connection or packet-fence assertion is needed for this test.
	require.NoError(t, m.tracker.refreshActiveConnections(ctx, connections, m))
	require.NoError(t, m.AddUpstreamProxyIPs(ctx, upstreamIPs))
	seeded := readQuiescenceKernelSets(t, ctx)
	require.ElementsMatch(t, []string{"192.0.2.10", "192.0.2.20", "192.0.2.30"}, seeded[dynAllowV4Set])
	require.ElementsMatch(t, []string{"2001:db8::10", "2001:db8::20", "2001:db8::30"}, seeded[dynAllowV6Set])
	require.Equal(t, []string{"192.0.2.40"}, seeded[upstreamProxyV4Set])
	require.Equal(t, []string{"2001:db8::40"}, seeded[upstreamProxyV6Set])
	require.Equal(t, []string{"198.51.100.10"}, seeded[allowV4Set])

	lookupEntered := make(chan context.Context, 1)
	releaseLookup, refreshDone := make(chan struct{}), make(chan struct{})
	lookupResult := make(chan error, 1)
	unblockLookup := sync.OnceFunc(func() { close(releaseLookup) })
	defer unblockLookup()
	go func() {
		m.refreshDomains(ctx, func(lookupCtx context.Context, domain string) ([]ResolvedIP, error) {
			lookupEntered <- lookupCtx
			select {
			case <-releaseLookup:
				ips := append([]ResolvedIP(nil), domainIPs...)
				for index := range ips {
					ips[index].TTL = time.Minute
				}
				if err := lookupCtx.Err(); err != nil {
					lookupResult <- err
					return nil, err
				}
				lookupResult <- nil
				return ips, nil
			case <-lookupCtx.Done():
				lookupResult <- lookupCtx.Err()
				return nil, lookupCtx.Err()
			}
		})
		close(refreshDone)
	}()
	var delayedLookupCtx context.Context
	select {
	case delayedLookupCtx = <-lookupEntered: // DNS runs outside Manager.mu before the commit.
	case <-ctx.Done():
		t.Fatal("domain refresh did not reach the lookup barrier: ", ctx.Err())
	}
	beforeCommit := calls.Load()
	injectCommittedError.Store(true)
	require.ErrorIs(t, m.ApplyStatic(ctx, newPolicy), committedError)
	require.False(t, injectCommittedError.Load(), "injection requires a successful real nft commit")
	require.Equal(t, beforeCommit+1, calls.Load())
	frozenCalls := calls.Load()
	// Release and drain before any nft readback, keeping those subprocesses
	// outside the production lookup's five-second deadline.
	unblockLookup()
	select {
	case <-refreshDone:
	case <-ctx.Done():
		t.Fatal("late domain refresh did not finish: ", ctx.Err())
	}
	require.NoError(t, <-lookupResult, "late DNS success must run before its lookup deadline")
	// refreshDomains cancels this context after its post-lookup Err check.
	// DeadlineExceeded is sticky, so Canceled proves that check preceded the
	// deadline, without relying on when this test goroutine gets scheduled.
	require.ErrorIs(t, delayedLookupCtx.Err(), context.Canceled, "production must consume a successful late lookup before its deadline")
	assertFrozenKernel := func(stage string) {
		t.Helper()
		assert.Equal(t, frozenCalls, calls.Load(), "%s submitted an nft write after freezing", stage)
		sets := readQuiescenceKernelSets(t, ctx)
		for _, name := range []string{dynAllowV4Set, dynAllowV6Set, upstreamProxyV4Set, upstreamProxyV6Set} {
			assert.Empty(t, sets[name], "%s restored old addresses to %s", stage, name)
		}
		assert.Equal(t, []string{"198.51.100.20"}, sets[allowV4Set], "%s replaced the committed static policy", stage)
	}
	assertFrozenKernel("committed static error")
	assertFrozenKernel("late domain result")
	assert.ErrorIs(t, m.AddResolvedIPs(ctx, directIPs), ErrQuiesced)
	assertFrozenKernel("direct IP addition")
	assert.ErrorIs(t, m.AddResolvedDomain(ctx, "old.example.com", domainIPs), ErrQuiesced)
	assertFrozenKernel("old-domain answer")
	assert.ErrorIs(t, m.tracker.refreshActiveConnections(ctx, connections, m), ErrQuiesced)
	assertFrozenKernel("TCP renewal")
	assert.ErrorIs(t, m.AddUpstreamProxyIPs(ctx, upstreamIPs), ErrQuiesced)
	assertFrozenKernel("upstream refresh")
	assert.ErrorIs(t, m.ApplyStatic(ctx, oldPolicy), ErrQuiesced)
	assertFrozenKernel("static retry")
	t.Logf("real nft commit at runner call %d; all six later runtime paths left kernel sets unchanged", frozenCalls)
}

// readQuiescenceKernelSets reads actual nft JSON, including set elements that
// may be plain addresses or wrapped timeout elements. Missing sets or unknown
// element representations fail instead of being mistaken for empty sets.
func readQuiescenceKernelSets(t *testing.T, ctx context.Context) map[string][]string {
	t.Helper()
	snapshot, err := exec.CommandContext(ctx, "nft", "-j", "list", "table", "inet", tableName).CombinedOutput()
	require.NoError(t, err, "%s", snapshot)
	var ruleset struct {
		Nftables []struct {
			Set *struct {
				Name     string            `json:"name"`
				Elements []json.RawMessage `json:"elem"`
			} `json:"set"`
		} `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal(snapshot, &ruleset), "%s", snapshot)
	sets := make(map[string][]string)
	for _, entry := range ruleset.Nftables {
		if entry.Set == nil {
			continue
		}
		name := entry.Set.Name
		if name != dynAllowV4Set && name != dynAllowV6Set && name != upstreamProxyV4Set && name != upstreamProxyV6Set && name != allowV4Set {
			continue
		}
		sets[name] = []string{}
		for _, element := range entry.Set.Elements {
			value := element
			var address string
			if err := json.Unmarshal(value, &address); err != nil {
				var timed struct {
					Element *struct {
						Value json.RawMessage `json:"val"`
					} `json:"elem"`
				}
				require.NoError(t, json.Unmarshal(element, &timed), "unexpected nft element: %s", element)
				if timed.Element != nil {
					value = timed.Element.Value
				}
				if err := json.Unmarshal(value, &address); err != nil {
					var prefix struct {
						Prefix *struct {
							Address string `json:"addr"`
							Bits    int    `json:"len"`
						} `json:"prefix"`
					}
					require.NoError(t, json.Unmarshal(value, &prefix), "unexpected nft element: %s", element)
					require.NotNil(t, prefix.Prefix, "unexpected nft element: %s", element)
					parsed, err := netip.ParseAddr(prefix.Prefix.Address)
					require.NoError(t, err, "unexpected nft prefix: %s", element)
					require.Equal(t, parsed.BitLen(), prefix.Prefix.Bits, "expected a singleton nft address: %s", element)
					address = parsed.String()
				}
			}
			parsed, err := netip.ParseAddr(address)
			require.NoError(t, err, "unexpected nft address element: %s", element)
			sets[name] = append(sets[name], parsed.String())
		}
	}
	for _, name := range []string{dynAllowV4Set, dynAllowV6Set, upstreamProxyV4Set, upstreamProxyV6Set, allowV4Set} {
		require.Contains(t, sets, name, "missing nft set %s: %s", name, snapshot)
	}
	return sets
}

func isolatedNftTest(t *testing.T, ctx context.Context) bool {
	t.Helper()
	switch os.Getenv("OPENSANDBOX_NFT_TEST") {
	case "1":
		parentNetns, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		command := exec.CommandContext(ctx, "unshare", "--net", os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
		command.Env = append(os.Environ(), "OPENSANDBOX_NFT_TEST=netns", "OPENSANDBOX_NFT_TEST_PARENT_NETNS="+parentNetns)
		output, err := command.CombinedOutput()
		require.NoError(t, err, "isolated nft test failed: %s", output)
		t.Logf("%s", output)
		return false
	case "netns":
		parentNetns := os.Getenv("OPENSANDBOX_NFT_TEST_PARENT_NETNS")
		require.NotEmpty(t, parentNetns, "run with OPENSANDBOX_NFT_TEST=1 to create an isolated network namespace")
		currentNetns, err := os.Readlink("/proc/self/ns/net")
		require.NoError(t, err)
		require.NotEqual(t, parentNetns, currentNetns, "nft test must run in a separate network namespace")
		t.Logf("isolated network namespace: %s (parent %s)", currentNetns, parentNetns)
	default:
		t.Skip("kernel validation not executed: set OPENSANDBOX_NFT_TEST=1 with nft and network namespace permissions")
	}

	return true
}
