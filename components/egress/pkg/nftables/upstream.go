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

package nftables

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

// UpstreamProxyEndpoint describes the chained upstream CONNECT proxy the
// mitmproxy process dials. Under a deny-by-default dns+nft policy the proxy
// endpoint is infrastructure, not sandbox egress: it must be reachable by the
// mitmproxy UID only, and must never land in the sandbox allow sets (a plain
// allow-set entry would let sandbox code CONNECT the proxy directly to
// otherwise-denied destinations on an unintercepted port like 3128).
type UpstreamProxyEndpoint struct {
	Port int
	UID  uint32
	// Mark, when non-zero, scopes the accept to sockets carrying this SO_MARK
	// instead of UID (forward mode, OSEP-0030): the sandbox shares the netns
	// and could assume a UID, but cannot set a mark without CAP_NET_ADMIN/RAW.
	Mark uint32
	// IPs seeds the scoped sets for literal-IP endpoints. Hostname endpoints
	// start empty and are filled via AddUpstreamProxyIPs as the dnsproxy
	// resolves the infra domain.
	IPs []netip.Addr
}

const (
	upstreamProxyV4Set = "upstream_proxy_v4"
	upstreamProxyV6Set = "upstream_proxy_v6"
	telemetryV4Set     = "telemetry_v4"
	telemetryV6Set     = "telemetry_v6"
)

// buildUpstreamProxyStatic renders the scoped sets, literal-IP seed elements,
// and the uid/mark+dport-scoped accept rules. The rules sit before the DoT/DoH
// drops so infra reachability is unaffected by blocklist or deny sets.
func buildUpstreamProxyStatic(table string, ep *UpstreamProxyEndpoint) string {
	return buildScopedEndpointStatic(table, upstreamProxyV4Set, upstreamProxyV6Set, ep)
}

func buildTelemetryStatic(table string, ep *UpstreamProxyEndpoint) string {
	return buildScopedEndpointStatic(table, telemetryV4Set, telemetryV6Set, ep)
}

func buildScopedEndpointStatic(table, v4Set, v6Set string, ep *UpstreamProxyEndpoint) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags timeout; }\n", table, v4Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; flags timeout; }\n", table, v6Set)
	for _, ip := range ep.IPs {
		addr := ip.Unmap()
		var set string
		if addr.Is4() {
			set = v4Set
		} else if addr.Is6() {
			set = v6Set
		} else {
			continue
		}
		// permanent element (no timeout): literal endpoints are not DNS-learned
		fmt.Fprintf(&b, "add element inet %s %s { %s }\n", table, set, addr)
	}
	owner := fmt.Sprintf("meta skuid %d", ep.UID)
	if ep.Mark != 0 {
		owner = fmt.Sprintf("meta mark %#x", ep.Mark)
	}
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s tcp dport %d %s accept\n", table, chainName, v4Set, ep.Port, owner)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s tcp dport %d %s accept\n", table, chainName, v6Set, ep.Port, owner)
	return b.String()
}

// AddUpstreamProxyIPs feeds DNS-learned proxy addresses into the scoped sets.
// Elements carry the (clamped) answer TTL, like the sandbox dynamic sets.
func (m *Manager) AddUpstreamProxyIPs(ctx context.Context, ips []ResolvedIP) error {
	return m.addScopedEndpointIPs(ctx, m.opts.UpstreamProxy, upstreamProxyV4Set, upstreamProxyV6Set, telemetry.NftOpUpstreamProxyAdd, ips)
}

// AddTelemetryIPs feeds DNS-learned OTLP collector addresses into the
// mark-scoped telemetry sets (forward mode, OSEP-0030).
func (m *Manager) AddTelemetryIPs(ctx context.Context, ips []ResolvedIP) error {
	return m.addScopedEndpointIPs(ctx, m.opts.Telemetry, telemetryV4Set, telemetryV6Set, telemetry.NftOpTelemetryAdd, ips)
}

func (m *Manager) addScopedEndpointIPs(ctx context.Context, ep *UpstreamProxyEndpoint, v4Set, v6Set, metricOp string, ips []ResolvedIP) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced.Load() {
		return ErrQuiesced
	}
	if m.staticStateUncertain {
		return errStaticStateUncertain
	}
	if ep == nil {
		return nil
	}
	var script strings.Builder
	for _, r := range ips {
		addr := r.Addr.Unmap()
		var set string
		if addr.Is4() {
			set = v4Set
		} else if addr.Is6() {
			set = v6Set
		} else {
			continue
		}
		fmt.Fprintf(&script, "add element inet %s %s { %s }\n", tableName, set, addr)
		fmt.Fprintf(&script, "delete element inet %s %s { %s }\n", tableName, set, addr)
		fmt.Fprintf(&script, "add element inet %s %s { %s timeout %ds }\n", tableName, set, addr, int(ClampTTL(r.TTL)/time.Second))
	}
	if script.Len() == 0 {
		return nil
	}
	if _, err := m.run(ctx, script.String()); err != nil {
		telemetry.RecordNftablesUpdateFailed(metricOp)
		return err
	}
	telemetry.RecordNftablesUpdate()
	return nil
}
