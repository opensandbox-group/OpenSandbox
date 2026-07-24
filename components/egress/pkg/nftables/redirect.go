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
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// dnsRedirectTable is the dedicated nat table for the DNS redirect. It is kept
// separate from the inet filter table (manager.go) so DNS interception works
// even in dns-only mode, where the filter chain is not installed.
const dnsRedirectTable = "opensandbox_dns_redirect"

// dnsRedirectFamilies are the families a redirect table may have been created
// in by this or a previous version; all are cleaned up on setup/teardown.
var dnsRedirectFamilies = []string{"inet", "ip", "ip6"}

// dnsRedirectScript returns the add-only native-nft ruleset for the DNS
// redirect: OUTPUT :53 (udp/tcp) is DNAT-redirected to the local proxy port,
// after RETURN exceptions for the SO_MARK'd proxy upstream traffic (meta mark)
// and for any exempt destinations (upstream nameservers).
//
// The chain uses `type nat hook output priority -100`; under runsc the redir
// expression rewrites the destination to loopback (127.0.0.1/::1) in the output
// hook, matching iptables REDIRECT semantics. It is add-only: callers delete any
// stale table first (deleteRedirectTables) so the whole thing is idempotent.
func dnsRedirectScript(port int, exemptDst []netip.Addr) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", dnsRedirectTable)
	fmt.Fprintf(&b, "add chain ip %s output { type nat hook output priority -100; policy accept; }\n", dnsRedirectTable)
	fmt.Fprintf(&b, "add table ip6 %s\n", dnsRedirectTable)
	fmt.Fprintf(&b, "add chain ip6 %s output { type nat hook output priority -100; policy accept; }\n", dnsRedirectTable)
	for _, addr := range exemptDst {
		if addr.Is4() {
			fmt.Fprintf(&b, "add rule ip %s output udp dport 53 ip daddr %s return\n", dnsRedirectTable, addr.String())
			fmt.Fprintf(&b, "add rule ip %s output tcp dport 53 ip daddr %s return\n", dnsRedirectTable, addr.String())
		} else {
			fmt.Fprintf(&b, "add rule ip6 %s output udp dport 53 ip6 daddr %s return\n", dnsRedirectTable, addr.String())
			fmt.Fprintf(&b, "add rule ip6 %s output tcp dport 53 ip6 daddr %s return\n", dnsRedirectTable, addr.String())
		}
	}
	fmt.Fprintf(&b, "add rule ip %s output meta mark %s udp dport 53 return\n", dnsRedirectTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip %s output meta mark %s tcp dport 53 return\n", dnsRedirectTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip %s output udp dport 53 redirect to :%d\n", dnsRedirectTable, port)
	fmt.Fprintf(&b, "add rule ip %s output tcp dport 53 redirect to :%d\n", dnsRedirectTable, port)
	fmt.Fprintf(&b, "add rule ip6 %s output meta mark %s udp dport 53 return\n", dnsRedirectTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip6 %s output meta mark %s tcp dport 53 return\n", dnsRedirectTable, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip6 %s output udp dport 53 redirect to :%d\n", dnsRedirectTable, port)
	fmt.Fprintf(&b, "add rule ip6 %s output tcp dport 53 redirect to :%d\n", dnsRedirectTable, port)
	return b.String()
}

// SetupRedirect installs the native-nft DNS redirect: OUTPUT :53 (udp/tcp) ->
// port, with the SO_MARK (meta mark) bypass for the proxy's own upstream queries
// and per-destination RETURN exceptions for the exempt nameserver list.
func SetupRedirect(port int, exemptDst []netip.Addr) error {
	log.Infof("installing nft DNS redirect: OUTPUT port 53 -> %d (mark %s bypass)", port, constants.MarkHex)
	if err := setupRedirect(context.Background(), defaultRunner, port, exemptDst); err != nil {
		return err
	}
	log.Infof("DNS redirect installed successfully")
	return nil
}

func setupRedirect(ctx context.Context, run runner, port int, exemptDst []netip.Addr) error {
	if err := deleteRedirectTables(ctx, run); err != nil {
		return err
	}
	script := dnsRedirectScript(port, exemptDst)
	if output, err := run(ctx, script); err != nil {
		return fmt.Errorf("nft DNS redirect apply failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// RemoveRedirect deletes the DNS redirect tables; missing tables are ignored.
func RemoveRedirect(port int, exemptDst []netip.Addr) {
	if err := deleteRedirectTables(context.Background(), defaultRunner); err != nil {
		log.Warnf("nft DNS redirect remove (ignored): %v", err)
		return
	}
	log.Infof("nft DNS redirect removed")
}

// deleteRedirectTables removes the redirect table from every family it may have
// been created in, tolerating tables that do not exist.
func deleteRedirectTables(ctx context.Context, run runner) error {
	for _, family := range dnsRedirectFamilies {
		if err := deleteTableIfPresent(ctx, run, family, dnsRedirectTable); err != nil {
			return err
		}
	}
	return nil
}
