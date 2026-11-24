// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package nftables

import (
	"net/netip"
	"slices"

	"github.com/alibaba/opensandbox/egress/pkg/policy"
)

type tcpSpans []policy.PortRange // sorted, disjoint, immutable once published

var allTCP = tcpSpans{{Lo: 1, Hi: 65535}}

func tcpRuleSpans(ports []int, ranges []policy.PortRange) tcpSpans {
	if len(ports) == 0 && len(ranges) == 0 {
		return allTCP
	}
	// Already normalized range-only rules (including the operator floor) can be borrowed.
	if len(ports) == 0 {
		for i := 1; i < len(ranges); i++ {
			if ranges[i].Lo <= ranges[i-1].Hi+1 {
				return mergeTCP(slices.Clone(ranges))
			}
		}
		return ranges
	}
	out := make(tcpSpans, 0, len(ports)+len(ranges))
	out = append(out, ranges...)
	for _, port := range ports {
		out = append(out, policy.PortRange{Lo: port, Hi: port})
	}
	return mergeTCP(out)
}

func mergeTCP(spans tcpSpans) tcpSpans {
	slices.SortFunc(spans, func(a, b policy.PortRange) int { return a.Lo - b.Lo })
	out := spans[:0]
	for _, r := range spans {
		if n := len(out); n != 0 && r.Lo <= out[n-1].Hi+1 {
			out[n-1].Hi = max(out[n-1].Hi, r.Hi)
		} else {
			out = append(out, r)
		}
	}
	return out
}

func (a tcpSpans) union(b tcpSpans) tcpSpans {
	switch {
	case len(a) == 0 || b.coversBounds(a):
		return b
	case len(b) == 0 || a.coversBounds(b) || slices.Equal(a, b):
		return a
	}
	out := make(tcpSpans, len(a)+len(b))
	copy(out, a)
	copy(out[len(a):], b)
	return mergeTCP(out)
}

func (a tcpSpans) coversBounds(b tcpSpans) bool {
	return len(a) == 1 && len(b) != 0 && a[0].Lo <= b[0].Lo && a[0].Hi >= b[len(b)-1].Hi
}

// Skip unrelated ranges when comparing a local exception to a large floor.
func (s tcpSpans) from(port int) tcpSpans {
	if len(s) == 0 || s[0].Hi >= port {
		return s
	}
	i, _ := slices.BinarySearchFunc(s, port, func(r policy.PortRange, p int) int { return r.Hi - p })
	return s[i:]
}

func (a tcpSpans) intersect(b tcpSpans) tcpSpans {
	if a.coversBounds(b) {
		return b
	}
	if b.coversBounds(a) || slices.Equal(a, b) {
		return a
	}
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	b = b.from(a[0].Lo)
	if len(b) == 0 {
		return nil
	}
	a = a.from(b[0].Lo)
	var out tcpSpans
	for i, j := 0, 0; i < len(a) && j < len(b); {
		x, y := a[i], b[j]
		if lo, hi := max(x.Lo, y.Lo), min(x.Hi, y.Hi); lo <= hi {
			out = append(out, policy.PortRange{Lo: lo, Hi: hi})
		}
		if x.Hi <= y.Hi {
			i++
		} else {
			j++
		}
	}
	return out
}

func (a tcpSpans) subtract(b tcpSpans) tcpSpans {
	if len(a) == 0 || len(b) == 0 {
		return a
	}
	if b.coversBounds(a) || slices.Equal(a, b) {
		return nil
	}
	b = b.from(a[0].Lo)
	var out tcpSpans
	j, changed := 0, false
	for i, r := range a {
		for j < len(b) && b[j].Hi < r.Lo {
			j++
		}
		if j == len(b) || b[j].Lo > r.Hi {
			if changed {
				out = append(out, r)
			}
			continue
		}
		if !changed {
			out, changed = append(out, a[:i]...), true
		}
		for j < len(b) && b[j].Lo <= r.Hi {
			if b[j].Lo > r.Lo {
				out = append(out, policy.PortRange{Lo: r.Lo, Hi: b[j].Lo - 1})
			}
			r.Lo = b[j].Hi + 1
			if r.Lo > r.Hi {
				break
			}
			j++
		}
		if r.Lo <= r.Hi {
			out = append(out, r)
		}
	}
	if !changed {
		return a
	}
	return out
}

func (a tcpSpans) contains(port int) bool {
	i, _ := slices.BinarySearchFunc(a, port, func(r policy.PortRange, p int) int { return r.Hi - p })
	return i < len(a) && a[i].Lo <= port
}

func tcpPrefix(target string) netip.Prefix {
	return teardownSelectorFor(target).prefix
}

type hostSpans struct {
	spans tcpSpans
	merge bool
}

func (group *hostSpans) add(spans tcpSpans) {
	switch {
	case len(group.spans) == 0 || spans.coversBounds(allTCP):
		// Borrow a single rule; append must allocate before we normalize.
		*group = hostSpans{spans: slices.Clip(spans)}
	case !group.merge && group.spans.coversBounds(spans):
		return
	default:
		group.spans = append(group.spans, spans...)
		group.merge = true
	}
}

func (group *hostSpans) normalized() tcpSpans {
	if group.merge {
		group.spans = mergeTCP(group.spans)
		group.merge = false
	}
	return group.spans
}
