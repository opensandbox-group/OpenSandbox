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

package dnsproxy

import (
	"net/netip"
	"os"
	"strings"
	"sync"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

var (
	exemptListOnce sync.Once
	exemptAddrs    []netip.Addr
	exemptSet      map[netip.Addr]struct{}
)

// ParseNameserverExemptList returns IPs from OPENSANDBOX_EGRESS_NAMESERVER_EXEMPT (comma-separated);
// only single-IP entries are kept; list is parsed once and cached. Used for nft, iptables RETURN, and dialer.
func ParseNameserverExemptList() []netip.Addr {
	exemptListOnce.Do(func() { parseNameserverExemptListUncached() })
	return exemptAddrs
}

func parseNameserverExemptListUncached() {
	raw := strings.TrimSpace(os.Getenv(constants.EnvNameserverExempt))
	if raw == "" {
		exemptAddrs = nil
		exemptSet = nil
		return
	}
	set := make(map[netip.Addr]struct{})
	var out []netip.Addr
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if addr, err := netip.ParseAddr(s); err == nil {
			if _, exists := set[addr]; exists {
				continue
			}
			set[addr] = struct{}{}
			out = append(out, addr)
		}
	}
	exemptAddrs = out
	exemptSet = set
}

// If true, dialer omits SO_MARK; exempt dst also skips REDIRECT in iptables.
func UpstreamInExemptList(upstreamHost string) bool {
	addr, err := netip.ParseAddr(upstreamHost)
	if err != nil {
		return false
	}
	ParseNameserverExemptList() // ensure cache is initialized
	_, ok := exemptSet[addr]
	return ok
}
