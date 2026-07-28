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

package telemetry

import (
	"net"
	"net/http"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// exporterHTTPClient returns an HTTP client whose connections carry SO_MARK.
//
// The egress sidecar shares a network namespace with the sandbox container, so it is
// subject to the very policy it installs: under `defaultAction: deny` its own OTLP
// export would be dropped by the nft output chain, and its DNS lookups redirected to
// the policy proxy (which answers NXDOMAIN for anything not in the allowlist).
//
// Marking is the same escape hatch the DNS proxy already uses for its upstream queries
// (see pkg/dnsproxy/proxy_linux.go): the nft chain has `meta mark <mark> accept` and the
// DNS redirect has a matching RETURN, so marked traffic bypasses both.
//
// Marking rather than allowlisting the collector is deliberate. The allow set is
// namespace-wide, so adding the collector there would also expose it to the untrusted
// sandbox container — and OTLP attributes are arbitrary strings, i.e. an exfiltration
// channel into the metrics backend. SO_MARK needs CAP_NET_ADMIN, which the server drops
// from the sandbox container precisely so only the sidecar can touch the network stack,
// so the workload cannot forge this.
func exporterHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	return &http.Client{Transport: transport}
}
