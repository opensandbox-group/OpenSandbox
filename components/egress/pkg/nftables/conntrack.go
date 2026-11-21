// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"net/netip"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

func countMatchingConntrackFlowsData(data []byte, drops EstablishedDrops) int {
	if len(data) == 0 || drops.Empty() {
		return 0
	}
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
