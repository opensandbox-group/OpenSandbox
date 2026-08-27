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

package opensandbox

import (
	"net"
	"strings"
	"sync"
)

// clientIPHeader carries the SDK host's own IP address to the server. A custom
// (non-standard) name is used on purpose: standard forwarded headers such as
// X-Forwarded-For are rewritten or stripped by intermediaries, so a dedicated
// name is needed to convey the client's self-reported IP reliably.
const clientIPHeader = "OPEN-SANDBOX-CLIENT-IP"

// virtualNICPrefixes are interface name fragments that indicate virtual,
// container, or VPN NICs whose address does not identify the host machine.
// They are skipped when picking the host's own IP.
var virtualNICPrefixes = []string{
	"docker", "veth", "br-", "virbr", "vmnet", "vbox",
	"utun", "tun", "tap", "zt", "cni", "flannel", "cali",
}

// clientIPDetector resolves the local intranet IP. It is a package variable so
// tests can stub the detection result. Returns "" when the IP cannot be
// determined (best-effort: callers must tolerate an empty result).
var clientIPDetector = detectOutboundIP

var (
	clientIPOnce  sync.Once
	clientIPValue string
)

// detectedClientIP returns the local intranet IP, detected once and cached.
func detectedClientIP() string {
	clientIPOnce.Do(func() {
		clientIPValue = clientIPDetector()
	})
	return clientIPValue
}

// nicAddrs is one network interface and its addresses; used so the selection
// logic can be unit-tested without real interfaces.
type nicAddrs struct {
	name string
	ips  []net.IP
}

// detectOutboundIP returns the host's own intranet IPv4 by reading the address
// configured on a real network interface (NOT by dialing out): the outbound
// route can traverse an egress cluster, whose source IP would not identify the
// host. Returns "" if no usable address is found.
func detectOutboundIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	nics := make([]nicAddrs, 0, len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		ips := make([]net.IP, 0, len(addrs))
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				ips = append(ips, ipNet.IP)
			}
		}
		nics = append(nics, nicAddrs{name: iface.Name, ips: ips})
	}
	return selectClientIP(nics)
}

// isUsableIPv4 reports whether ip is a routable-on-a-LAN unicast IPv4 address
// (excludes loopback, link-local 169.254.x, and unspecified).
func isUsableIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	return !v4.IsLoopback() && !v4.IsLinkLocalUnicast() && !v4.IsUnspecified()
}

// nicNameRank scores an interface name by how likely it is the host's primary
// intranet NIC (lower is better). Named NICs beat unknown ones.
func nicNameRank(name string) int {
	name = strings.ToLower(name)
	switch {
	case name == "en0":
		return 0
	case name == "eth0":
		return 1
	case strings.HasPrefix(name, "eth"):
		return 2
	case strings.HasPrefix(name, "en"): // covers en*, ens*, enp*, eno*
		return 3
	case strings.HasPrefix(name, "bond"):
		return 4
	default:
		return 100
	}
}

func isVirtualNIC(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range virtualNICPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// selectClientIP picks the host's intranet IPv4 from the given interfaces.
// Selection prefers, in order: known NIC names (nicNameRank), then RFC1918
// private addresses. This yields "network-name priority with private-segment
// fallback" in a single pass. Virtual/container/VPN NICs are skipped. Returns
// "" if no usable address exists.
func selectClientIP(nics []nicAddrs) string {
	best := ""
	bestRank := 0
	bestPrivate := false
	found := false

	for _, nic := range nics {
		if isVirtualNIC(nic.name) {
			continue
		}
		rank := nicNameRank(nic.name)
		for _, ip := range nic.ips {
			if !isUsableIPv4(ip) {
				continue
			}
			private := ip.IsPrivate()
			// Better = lower rank; on equal rank, private beats non-private.
			if !found || rank < bestRank || (rank == bestRank && private && !bestPrivate) {
				best = ip.String()
				bestRank = rank
				bestPrivate = private
				found = true
			}
		}
	}
	return best
}

// applyClientIP adds the client IP header to headers unless the caller already
// provided it (matched case-insensitively). An empty ip is a no-op. It returns
// the possibly-newly-allocated map so callers can reassign.
func applyClientIP(headers map[string]string, ip string) map[string]string {
	if ip == "" {
		return headers
	}
	for k := range headers {
		if strings.EqualFold(k, clientIPHeader) {
			// Respect a user-supplied value; do not overwrite.
			return headers
		}
	}
	if headers == nil {
		headers = make(map[string]string, 1)
	}
	headers[clientIPHeader] = ip
	return headers
}
