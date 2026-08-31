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

// Gateway DNS redirect (fleet profile): the shared DNS proxy binds loopback
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
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

const gatewayDNSNftTable = "opensandbox_gateway_dns"

func gatewayDNSRedirectScript(gateway netip.Addr, port int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table inet %s\n", gatewayDNSNftTable)
	fmt.Fprintf(&b, "add chain inet %s gw { type nat hook prerouting priority dstnat; }\n", gatewayDNSNftTable)
	addr := gateway.String()
	udp := fmt.Sprintf("udp dport 53 redirect to :%d", port)
	tcp := fmt.Sprintf("tcp dport 53 redirect to :%d", port)
	if gateway.Is4() {
		fmt.Fprintf(&b, "add rule inet %s gw ip daddr %s %s\n", gatewayDNSNftTable, addr, udp)
		fmt.Fprintf(&b, "add rule inet %s gw ip daddr %s %s\n", gatewayDNSNftTable, addr, tcp)
	} else {
		fmt.Fprintf(&b, "add rule inet %s gw ip6 daddr %s %s\n", gatewayDNSNftTable, addr, udp)
		fmt.Fprintf(&b, "add rule inet %s gw ip6 daddr %s %s\n", gatewayDNSNftTable, addr, tcp)
	}
	return b.String()
}

// SetupGatewayDNSRedirect installs (idempotent per gateway) the prerouting
// REDIRECT so DNS addressed to the sandbox gateway:53 reaches the local DNS
// proxy. Installing for an already-present gateway is a no-op.
func SetupGatewayDNSRedirect(gateway netip.Addr, port int) error {
	if !gateway.IsValid() || port <= 0 {
		return fmt.Errorf("gateway DNS redirect: invalid gateway %s or port %d", gateway, port)
	}
	r := defaultRedirectRunner()
	if out, err := r.runNft(context.Background(), "list table inet "+gatewayDNSNftTable); err == nil &&
		strings.Contains(string(out), gateway.String()) {
		return nil // already installed for this gateway
	}
	if _, err := r.runNft(context.Background(), gatewayDNSRedirectScript(gateway, port)); err != nil {
		return fmt.Errorf("gateway DNS redirect install: %w", err)
	}
	log.Infof("gateway DNS redirect installed (gateway %s -> :%d)", gateway, port)
	return nil
}

// RemoveGatewayDNSRedirect removes the whole gateway redirect table
// (idempotent; a missing table is not an error). Callers must refcount per
// gateway: several subjects may share one gateway.
func RemoveGatewayDNSRedirect() error {
	script := fmt.Sprintf("delete table inet %s\n", gatewayDNSNftTable)
	r := defaultRedirectRunner()
	if _, err := r.runNft(context.Background(), script); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist") {
			return nil
		}
		return fmt.Errorf("gateway DNS redirect remove: %w", err)
	}
	log.Infof("gateway DNS redirect removed")
	return nil
}
