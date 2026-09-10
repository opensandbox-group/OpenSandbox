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
	"fmt"
	"net/netip"
	"strings"
	"time"
)

const (
	dynAllowV4Set  = "dyn_allow_v4"
	dynAllowV6Set  = "dyn_allow_v6"
	dynSetTimeoutS = 360
	// nftTTLSlackSec is added to the DNS TTL before clamping, so allow entries
	// slightly outlive the resolver cache and reduce races with short TTLs.
	nftTTLSlackSec = 60
	minTTLSec      = 60
	maxTTLSec      = 360 // max DNS TTL (300) + nftTTLSlackSec
)

// ResolvedIP is a single IP learned from DNS with TTL for dynamic nft set.
type ResolvedIP struct {
	Addr netip.Addr
	TTL  time.Duration
}

// buildAddResolvedIPsScript returns a nft script fragment that
// adds resolved IPs to dyn_allow_v4/v6 with timeout.
func buildAddResolvedIPsScript(table string, ips []ResolvedIP) string {
	elements := make([]ResolvedIP, 0, len(ips))
	for _, r := range ips {
		elements = append(elements, ResolvedIP{
			Addr: r.Addr,
			TTL:  clampTTL(r.TTL),
		})
	}
	return buildResolvedIPElementsScript(table, elements)
}

func clampTTL(d time.Duration) time.Duration {
	sec := int(d.Seconds()) + nftTTLSlackSec
	sec = min(max(sec, minTTLSec), maxTTLSec)
	return time.Duration(sec) * time.Second
}

// buildRefreshResolvedIPsScript renders active connection refreshes with the
// full set timeout rather than the DNS TTL. Activity proves the address is
// still in use, and the final refresh after the connection closes makes this
// same bounded timeout the reconnect grace period.
func buildRefreshResolvedIPsScript(table string, ips []netip.Addr) string {
	elements := make([]ResolvedIP, 0, len(ips))
	for _, addr := range ips {
		elements = append(elements, ResolvedIP{
			Addr: addr,
			TTL:  dynSetTimeoutS * time.Second,
		})
	}
	return buildResolvedIPElementsScript(table, elements)
}

func buildResolvedIPElementsScript(table string, elements []ResolvedIP) string {
	var script strings.Builder
	for _, element := range elements {
		addr := element.Addr.Unmap()
		var setName string
		if addr.Is4() {
			setName = dynAllowV4Set
		} else if addr.Is6() {
			setName = dynAllowV6Set
		} else {
			continue
		}
		fmt.Fprintf(&script, "add element inet %s %s { %s }\n", table, setName, addr)
		fmt.Fprintf(&script, "delete element inet %s %s { %s }\n", table, setName, addr)
		fmt.Fprintf(&script, "add element inet %s %s { %s timeout %ds }\n", table, setName, addr, int(element.TTL/time.Second))
	}
	return script.String()
}
