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
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// countMatchingConntrackFlows reads /proc/net/nf_conntrack and counts TCP flows
// whose ORIGINAL destination matches drops. Best-effort: a missing proc file
// (no nf_conntrack loaded) returns 0, nil so policy changes still succeed.
func countMatchingConntrackFlows(drops EstablishedDrops) (int, error) {
	if drops.Empty() {
		return 0, nil
	}
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return countMatchingConntrackFlowsData(data, drops), nil
}

func countMatchingConntrackFlowsData(data []byte, drops EstablishedDrops) int {
	matched := 0
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 6 || f[2] != "tcp" {
			continue
		}
		var dst netip.Addr
		dport := 0
		marked := false
		for _, tok := range f[6:] {
			if v, ok := strings.CutPrefix(tok, "mark="); ok {
				// Marked proxy upstream flows are accepted ahead of teardown.
				// The chain matches `meta mark 0x1` exactly, so mirror that.
				if n, err := strconv.ParseUint(v, 0, 32); err == nil && n == constants.MarkValue {
					marked = true
				}
				continue
			}
			if v, ok := strings.CutPrefix(tok, "dst="); ok && !dst.IsValid() {
				if a, err := netip.ParseAddr(v); err == nil {
					dst = a.Unmap()
				}
				continue
			}
			if v, ok := strings.CutPrefix(tok, "dport="); ok && dport == 0 {
				if p, err := strconv.Atoi(v); err == nil {
					dport = p
				}
			}
		}
		if !marked && dst.IsValid() && drops.MatchesFlow(dst, dport) {
			matched++
		}
	}
	return matched
}

// conntrackEstablishedTimeoutPath is the kernel's idle timeout for established
// TCP conntrack entries; a var so tests can point it elsewhere.
var conntrackEstablishedTimeoutPath = "/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"

// establishedCarryTTL bounds how long a dropEstablished teardown set is
// carried across later full applies: once the conntrack established timeout
// has passed, any flow the drop targeted has been idle long enough to expire.
func establishedCarryTTL() time.Duration {
	data, err := os.ReadFile(conntrackEstablishedTimeoutPath)
	if err != nil {
		return defaultEstablishedCarryTTL
	}
	secs, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || secs <= 0 {
		return defaultEstablishedCarryTTL
	}
	return time.Duration(secs) * time.Second
}
