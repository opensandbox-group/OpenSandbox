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

package iptables

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// forwardRedirectNftTable captures all outbound IPv4 TCP for forward mode
// (OSEP-0030). IPv6 is intentionally not redirected: phase 1 drops it in the
// filter table instead of relaying.
const forwardRedirectNftTable = "opensandbox_forward_redirect"

// forwardRedirectNftScript redirects every new outbound IPv4 TCP connection to
// the forwarder, except loopback, the DNS proxy's own upstream traffic (mark
// 0x1, including DNS over TCP), the forwarder's gateway sockets (mark 0x4),
// the sidecar's trusted OTLP telemetry sockets (mark 0x8), link-local
// destinations (left for the filter table to drop) and port 53 (handled by
// the DNS redirect). Marks are trusted because setting one requires
// CAP_NET_ADMIN or CAP_NET_RAW, which forward mode denies the sandbox.
func forwardRedirectNftScript(port int) string {
	t := forwardRedirectNftTable
	var b strings.Builder
	fmt.Fprintf(&b, "delete table ip %s\n", t)
	fmt.Fprintf(&b, "add table ip %s\n", t)
	fmt.Fprintf(&b, "add chain ip %s output { type nat hook output priority -100; policy accept; }\n", t)
	fmt.Fprintf(&b, "add rule ip %s output ip daddr 127.0.0.0/8 return\n", t)
	fmt.Fprintf(&b, "add rule ip %s output meta mark %s return\n", t, constants.MarkHex)
	fmt.Fprintf(&b, "add rule ip %s output meta mark %s return\n", t, constants.ForwarderMarkHex)
	fmt.Fprintf(&b, "add rule ip %s output meta mark %s return\n", t, constants.TelemetryMarkHex)
	fmt.Fprintf(&b, "add rule ip %s output ip daddr 169.254.0.0/16 return\n", t)
	fmt.Fprintf(&b, "add rule ip %s output tcp dport 53 return\n", t)
	fmt.Fprintf(&b, "add rule ip %s output meta l4proto tcp redirect to :%d\n", t, port)
	return b.String()
}

func (r redirectRunner) setupForwardRedirect(ctx context.Context, port int) error {
	script := forwardRedirectNftScript(port)
	output, err := r.runNft(ctx, script)
	if err != nil && isNftMissingTableErrorFor(output, err, forwardRedirectNftTable) {
		output, err = r.runNft(ctx, removeNftDeleteTableLineFor(script, forwardRedirectNftTable))
	}
	if err != nil {
		return fmt.Errorf("nft forward redirect failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// SetupForwardRedirect installs the forward-mode TCP capture. There is no
// remove counterpart: the capture is retained on shutdown so a stopped
// forwarder leaves egress blocked rather than open.
func SetupForwardRedirect(port int) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("forward redirect: only supported on linux")
	}
	if port <= 0 || port > 65535 {
		return fmt.Errorf("forward redirect: invalid port %d", port)
	}
	if err := defaultRedirectRunner().setupForwardRedirect(context.Background(), port); err != nil {
		return err
	}
	log.Infof("nft forward redirect installed: OUTPUT ip tcp -> :%d", port)
	return nil
}
