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
	var v4, v6 []string
	for _, r := range ips {
		sec := clampTTL(r.TTL)
		if r.Addr.Is4() {
			v4 = append(v4, fmt.Sprintf("%s timeout %ds", r.Addr.String(), sec))
		} else if r.Addr.Is6() {
			v6 = append(v6, fmt.Sprintf("%s timeout %ds", r.Addr.String(), sec))
		}
	}
	var b strings.Builder
	if len(v4) > 0 {
		fmt.Fprintf(&b, "add element inet %s %s { %s }\n", table, dynAllowV4Set, strings.Join(v4, ", "))
	}
	if len(v6) > 0 {
		fmt.Fprintf(&b, "add element inet %s %s { %s }\n", table, dynAllowV6Set, strings.Join(v6, ", "))
	}
	return b.String()
}

func clampTTL(d time.Duration) int {
	sec := int(d.Seconds()) + nftTTLSlackSec
	if sec < minTTLSec {
		return minTTLSec
	}
	if sec > maxTTLSec {
		return maxTTLSec
	}
	return sec
}
