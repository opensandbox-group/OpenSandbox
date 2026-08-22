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

package credentialvault

import (
	"net/http"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

func httpSourceTransport() http.RoundTripper {
	dialer := defaultTransportDialer()
	dialer.Control = func(network, address string, c syscall.RawConn) error {
		_ = c.Control(func(fd uintptr) {
			// Best-effort: requires CAP_NET_ADMIN. In environments without
			// iptables transparent redirect (tests, dev), the mark is
			// unnecessary and the error is harmless.
			_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, constants.CredentialFetchMarkValue)
		})
		return nil
	}
	return &http.Transport{DialContext: dialer.DialContext}
}
