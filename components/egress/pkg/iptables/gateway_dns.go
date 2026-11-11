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

// Gateway DNS redirect (fast-sandbox profile): the shared DNS proxy binds loopback
// (127.0.0.1:15353) so it never collides with a host DNS service on :53,
// while sandboxes resolve against slot.Gateway:53 (resolv.conf rewrite).
// A prerouting REDIRECT per gateway forwards that traffic to the loopback
// proxy. REDIRECT rewrites only the destination — the source IP is
// preserved, so per-subject DNS dispatch keeps working.
package iptables

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

const gatewayDNSNftTable = "opensandbox_gateway_dns"

// gatewayDNSRedirectScript builds one transactional replacement of the
// shared table. Empty gateways means remove the table; addresses are
// normalized and deduplicated before rendering so mapped IPv4 values cannot
// create a second spelling of the same gateway.
func gatewayDNSRedirectScript(gateways []netip.Addr, port int) (string, int, error) {
	if port <= 0 {
		return "", 0, fmt.Errorf("gateway DNS redirect: invalid port %d", port)
	}
	unique := make(map[netip.Addr]struct{}, len(gateways))
	for _, gateway := range gateways {
		if !gateway.IsValid() {
			return "", 0, fmt.Errorf("gateway DNS redirect: invalid gateway %s", gateway)
		}
		unique[gateway.Unmap()] = struct{}{}
	}
	normalized := make([]netip.Addr, 0, len(unique))
	for gateway := range unique {
		normalized = append(normalized, gateway)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Compare(normalized[j]) < 0 })

	var b strings.Builder
	fmt.Fprintf(&b, "delete table inet %s\n", gatewayDNSNftTable)
	if len(normalized) == 0 {
		return b.String(), len(normalized), nil
	}
	fmt.Fprintf(&b, "add table inet %s\n", gatewayDNSNftTable)
	fmt.Fprintf(&b, "add chain inet %s gw { type nat hook prerouting priority dstnat; }\n", gatewayDNSNftTable)
	for _, gateway := range normalized {
		family, addr := "ip6", gateway.String()
		if gateway.Is4() {
			family = "ip"
		}
		fmt.Fprintf(&b, "add rule inet %s gw %s daddr %s udp dport 53 redirect to :%d\n", gatewayDNSNftTable, family, addr, port)
		fmt.Fprintf(&b, "add rule inet %s gw %s daddr %s tcp dport 53 redirect to :%d\n", gatewayDNSNftTable, family, addr, port)
	}
	return b.String(), len(normalized), nil
}

// ReplaceGatewayDNSRedirects atomically replaces the shared prerouting
// redirect table with exactly the supplied gateway set. Callers own the
// subject-to-gateway map and must retain it for a retry when this returns an
// error; an ambiguous command result does not prove the old table is live.
func ReplaceGatewayDNSRedirects(ctx context.Context, gateways []netip.Addr, port int) error {
	return replaceGatewayDNSRedirects(ctx, defaultRedirectRunner(), gateways, port)
}

func replaceGatewayDNSRedirects(ctx context.Context, r redirectRunner, gateways []netip.Addr, port int) error {
	script, uniqueGatewayCount, err := gatewayDNSRedirectScript(gateways, port)
	if err != nil {
		return err
	}
	out, err := r.runNft(ctx, script)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("gateway DNS redirect replace: %w (output: %s)", ctxErr, strings.TrimSpace(string(out)))
		}
		if !isNftMissingTableErrorFor(out, err, gatewayDNSNftTable) {
			return fmt.Errorf("gateway DNS redirect replace: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}
		if len(gateways) == 0 {
			return nil
		}
		retryScript := removeNftDeleteTableLineFor(script, gatewayDNSNftTable)
		retryOut, retryErr := r.runNft(ctx, retryScript)
		if retryErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("gateway DNS redirect replace retry: %w (output: %s)", ctxErr, strings.TrimSpace(string(retryOut)))
			}
			return fmt.Errorf("gateway DNS redirect replace retry: %w (output: %s)", retryErr, strings.TrimSpace(string(retryOut)))
		}
	}
	log.Infof("gateway DNS redirects replaced (%d unique gateway(s) -> :%d)", uniqueGatewayCount, port)
	return nil
}
