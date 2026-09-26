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

package main

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/mitmproxy"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
)

func TestUpstreamProxySpecForProfile(t *testing.T) {
	tests := []struct {
		name        string
		proxy       string
		auth        string
		transparent string
		profile     string
		mode        string
		wantSpec    bool
		wantErrSubs []string
	}{
		{
			name: "no proxy env and transparent off is inert",
		},
		{
			name:     "proxy without transparent fails",
			proxy:    "http://127.0.0.1:3128",
			wantSpec: false,
			wantErrSubs: []string{
				constants.EnvUpstreamProxy,
				constants.EnvMitmproxyTransparent,
			},
		},
		{
			name: "auth without proxy fails even when transparent off",
			auth: "Basic dGVzdDp0ZXN0",
			wantErrSubs: []string{
				constants.EnvUpstreamProxyAuth,
				constants.EnvUpstreamProxy,
			},
		},
		{
			name:        "malformed proxy fails even when transparent off",
			proxy:       "://not-a-url",
			wantErrSubs: []string{constants.EnvUpstreamProxy},
		},
		{
			name:        "proxy with transparent and dns+nft succeeds for default profile",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			mode:        constants.PolicyDnsNft,
			wantSpec:    true,
		},
		{
			name:        "proxy with transparent on succeeds for sidecar profile",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			profile:     "sidecar",
			mode:        constants.PolicyDnsNft,
			wantSpec:    true,
		},
		{
			name:        "proxy requires dns+nft enforcement",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			wantErrSubs: []string{constants.EnvUpstreamProxy, constants.EnvEgressMode, constants.PolicyDnsNft},
		},
		{
			name:        "proxy accepts explicit dns+nft enforcement",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			mode:        constants.PolicyDnsNft,
			wantSpec:    true,
		},
		{
			name:        "proxy with transparent on succeeds for fast-sandbox profile without mode env",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			profile:     constants.ProfileFastSandbox,
			wantSpec:    true,
		},
		{
			name:        "proxy with transparent on succeeds for fast-sandbox profile with sidecar mode env",
			proxy:       "http://proxy.local:3128",
			transparent: "true",
			profile:     constants.ProfileFastSandbox,
			mode:        constants.PolicyDnsNft,
			wantSpec:    true,
		},
		{
			name:        "proxy without transparent fails under fast-sandbox profile",
			proxy:       "http://proxy.local:3128",
			profile:     constants.ProfileFastSandbox,
			wantErrSubs: []string{
				constants.EnvUpstreamProxy,
				constants.EnvMitmproxyTransparent,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvUpstreamProxy, tc.proxy)
			t.Setenv(constants.EnvUpstreamProxyAuth, tc.auth)
			t.Setenv(constants.EnvMitmproxyTransparent, tc.transparent)
			t.Setenv(constants.EnvEgressMode, tc.mode)

			spec, err := upstreamProxySpecForProfile(tc.profile)
			if len(tc.wantErrSubs) > 0 {
				if err == nil {
					t.Fatalf("expected error containing %v, got nil", tc.wantErrSubs)
				}
				for _, sub := range tc.wantErrSubs {
					if !strings.Contains(err.Error(), sub) {
						t.Fatalf("error %q missing %q", err.Error(), sub)
					}
				}
				if spec != nil {
					t.Fatalf("expected nil spec on error, got %+v", spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantSpec {
				if spec == nil {
					t.Fatal("expected parsed spec, got nil")
				}
				if spec.Host != "proxy.local" || spec.Port != 3128 {
					t.Fatalf("unexpected spec %+v", spec)
				}
			} else if spec != nil {
				t.Fatalf("expected nil spec, got %+v", spec)
			}
		})
	}
}

func TestFastSandboxUpstreamEndpoint(t *testing.T) {
	if got := fastSandboxUpstreamEndpoint(nil); got != nil {
		t.Fatalf("nil spec must yield nil endpoint, got %+v", got)
	}

	literal := fastSandboxUpstreamEndpoint(&mitmproxy.UpstreamProxySpec{Scheme: "http", Host: "10.1.2.3", Port: 3128})
	if literal == nil || literal.Port != 3128 {
		t.Fatalf("unexpected literal endpoint %+v", literal)
	}
	want := []netip.Addr{netip.MustParseAddr("10.1.2.3")}
	if len(literal.LiteralIPs) != 1 || literal.LiteralIPs[0] != want[0] {
		t.Fatalf("literal endpoint must seed the drop set, got %+v", literal.LiteralIPs)
	}

	hostname := fastSandboxUpstreamEndpoint(&mitmproxy.UpstreamProxySpec{Scheme: "https", Host: "proxy.example.com", Port: 8443})
	if hostname == nil || hostname.Port != 8443 || len(hostname.LiteralIPs) != 0 {
		t.Fatalf("hostname endpoint must start empty for DNS learning, got %+v", hostname)
	}
}

func TestUnionResolvedIPs(t *testing.T) {
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")
	ips := unionResolvedIPs([]nftables.ResolvedIP{
		{Addr: a, TTL: 5 * time.Minute},
		{Addr: b}, // pod-resolver form: no TTL
		{Addr: a}, // duplicate, must not shorten the dnsproxy TTL
	})
	if len(ips) != 2 {
		t.Fatalf("expected dedupe to 2 entries, got %+v", ips)
	}
	if ips[0].Addr != a || ips[0].TTL != 5*time.Minute {
		t.Fatalf("TTL-bearing entry must win for %s, got %+v", a, ips[0])
	}
	if ips[1].Addr != b {
		t.Fatalf("unexpected second entry %+v", ips[1])
	}
}

func TestUnionResolver(t *testing.T) {
	ctx := context.Background()
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")
	returning := func(ips ...nftables.ResolvedIP) func(context.Context, string) ([]nftables.ResolvedIP, error) {
		return func(context.Context, string) ([]nftables.ResolvedIP, error) { return ips, nil }
	}
	failing := func(err error) func(context.Context, string) ([]nftables.ResolvedIP, error) {
		return func(context.Context, string) ([]nftables.ResolvedIP, error) { return nil, err }
	}

	t.Run("unions both authorities", func(t *testing.T) {
		ips, err := unionResolver(ctx, "proxy.test",
			returning(nftables.ResolvedIP{Addr: a, TTL: time.Minute}),
			returning(nftables.ResolvedIP{Addr: b}))
		if err != nil || len(ips) != 2 {
			t.Fatalf("expected union of both authorities, got %+v err %v", ips, err)
		}
	})

	t.Run("tolerates one failing authority", func(t *testing.T) {
		ips, err := unionResolver(ctx, "proxy.test",
			failing(context.DeadlineExceeded),
			returning(nftables.ResolvedIP{Addr: b}))
		if err != nil || len(ips) != 1 || ips[0].Addr != b {
			t.Fatalf("expected surviving authority's answers, got %+v err %v", ips, err)
		}
	})

	t.Run("all authorities failing is an error naming each", func(t *testing.T) {
		_, err := unionResolver(ctx, "proxy.test",
			failing(context.DeadlineExceeded),
			failing(context.Canceled))
		if err == nil {
			t.Fatal("every authority failing must be an error")
		}
		if !strings.Contains(err.Error(), "deadline") || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("error must carry each authority's failure, got %q", err.Error())
		}
	})

	t.Run("NXDOMAIN everywhere is empty, not an error", func(t *testing.T) {
		ips, err := unionResolver(ctx, "gone.test", returning(), returning())
		if err != nil || len(ips) != 0 {
			t.Fatalf("expected empty union without error, got %+v err %v", ips, err)
		}
	})
}

func TestResolveUpstreamProxyHost(t *testing.T) {
	// The split-authority case from the review: the dnsproxy's forward
	// upstream errors (e.g. OPENSANDBOX_EGRESS_DNS_UPSTREAM pointing at a
	// resolver with a different view) while the Pod resolver — mitmdump's
	// dial authority — still resolves the name. "localhost" resolves locally
	// on every platform, so no external network is touched.
	ips, err := resolveUpstreamProxyHost(context.Background(), "localhost",
		func(context.Context, string) ([]nftables.ResolvedIP, error) {
			return nil, context.DeadlineExceeded
		})
	if err != nil {
		t.Fatalf("a failing dns authority must not fail the union: %v", err)
	}
	if len(ips) == 0 {
		t.Fatal("the pod resolver must cover the failing dns authority")
	}
}
