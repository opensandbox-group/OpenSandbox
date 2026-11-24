// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import "net/netip"

func tcpPrefix(target string) netip.Prefix {
	if a, err := netip.ParseAddr(target); err == nil {
		return netip.PrefixFrom(a, a.BitLen())
	}
	p, _ := netip.ParsePrefix(target)
	return p.Masked()
}
