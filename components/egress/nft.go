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

package main

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/dnsproxy"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

// createNftManager is non-nil only when mode includes the nft token (e.g. dns+nft).
func createNftManager(mode string) nftApplier {
	if !constants.ModeUsesNft(mode) {
		return nil
	}
	return nftables.NewManagerWithOptions(parseNftOptions())
}

// setupNft: apply static policy to nft, then wire allowed DNS answers to AddResolvedIPs (dynamic allow sets).
// nameserverIPs and always-deny/allow follow the same merge rules as the policy API (MergeAlwaysOverlay + WithExtraAllowIPs).
func setupNft(ctx context.Context, nftMgr nftApplier, initialPolicy *policy.NetworkPolicy, proxy *dnsproxy.Proxy, nameserverIPs []netip.Addr, alwaysDeny, alwaysAllow []policy.EgressRule) {
	if nftMgr == nil {
		log.Warnf("nftables disabled (dns-only mode)")
		return
	}

	log.Infof("applying nftables static policy (dns+nft mode) with %d nameserver IP(s) merged into allow set", len(nameserverIPs))
	merged := policy.MergeAlwaysOverlay(initialPolicy, alwaysDeny, alwaysAllow)
	policyWithNS := merged.WithExtraAllowIPs(nameserverIPs)
	if err := nftMgr.ApplyStatic(ctx, policyWithNS); err != nil {
		log.Fatalf("nftables static apply failed: %v", err)
	}
	log.Infof("nftables static policy applied (table inet opensandbox); DNS-resolved IPs will be added to dynamic allow sets")
	proxy.SetOnResolved(func(domain string, ips []nftables.ResolvedIP) {
		addCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := nftMgr.AddResolvedIPs(addCtx, ips); err != nil {
			log.Warnf("[dns] add resolved IPs to nft failed for domain %q: %v", domain, err)
		}
	})
}

func parseNftOptions() nftables.Options {
	opts := nftables.Options{BlockDoT: true}
	if constants.IsTruthy(os.Getenv(constants.EnvBlockDoH443)) {
		opts.BlockDoH443 = true
	}
	if raw := os.Getenv(constants.EnvDoHBlocklist); strings.TrimSpace(raw) != "" {
		parts := strings.Split(raw, ",")
		for _, p := range parts {
			target := strings.TrimSpace(p)
			if target == "" {
				continue
			}
			if addr, err := netip.ParseAddr(target); err == nil {
				if addr.Is4() {
					opts.DoHBlocklistV4 = append(opts.DoHBlocklistV4, target)
				} else if addr.Is6() {
					opts.DoHBlocklistV6 = append(opts.DoHBlocklistV6, target)
				}
				continue
			}
			if prefix, err := netip.ParsePrefix(target); err == nil {
				if prefix.Addr().Is4() {
					opts.DoHBlocklistV4 = append(opts.DoHBlocklistV4, target)
				} else if prefix.Addr().Is6() {
					opts.DoHBlocklistV6 = append(opts.DoHBlocklistV6, target)
				}
				continue
			}
			log.Warnf("ignoring invalid DoH blocklist entry: %s", target)
		}
	}
	return opts
}
