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

//go:build linux

package dnsproxy

import (
	"net"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

var exemptDialerLogOnce sync.Once

// dialerForUpstream sets SO_MARK so iptables can RETURN marked packets (bypass
// redirect for proxy's own upstream DNS queries). When upstream is in the nameserver
// exempt list, returns a plain dialer (no mark) so upstream traffic follows normal
// routing (e.g. via tun); iptables still does not redirect by destination exempt.
func (p *Proxy) dialerForUpstream(upstreamAddr string) *net.Dialer {
	host, _, err := net.SplitHostPort(upstreamAddr)
	if err != nil {
		host = upstreamAddr
	}
	if UpstreamInExemptList(host) {
		exemptDialerLogOnce.Do(func() {
			log.Infof("[dns] upstream %s in nameserver exempt list, not setting SO_MARK", host)
		})
		return &net.Dialer{Timeout: p.upstreamExchangeTimeout}
	}

	return &net.Dialer{
		Timeout: p.upstreamExchangeTimeout,
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			if err := c.Control(func(fd uintptr) {
				opErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, constants.MarkValue)
			}); err != nil {
				return err
			}
			return opErr
		},
	}
}
