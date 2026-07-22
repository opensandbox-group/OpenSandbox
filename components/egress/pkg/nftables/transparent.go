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

package nftables

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// transparentTable is the dedicated nat table for the transparent-HTTP redirect
// used by the MITM proxy. Like the DNS redirect it is separate from the inet
// filter table. IPv4-only, mirroring the original iptables rule set.
const transparentTable = "opensandbox_transparent"

// transparentScript returns the add-only native-nft ruleset for the transparent
// HTTP intercept: TCP :80 and :443 from any UID other than the MITM proxy's own
// are DNAT-redirected to the local proxy port. Loopback destinations and the
// proxy's own upstream connections (matched by meta skuid == mitmUID) are left
// untouched so the proxy can reach real servers without a redirect loop.
func transparentScript(localPort int, mitmUID uint32) string {
	var b strings.Builder
	fmt.Fprintf(&b, "add table ip %s\n", transparentTable)
	fmt.Fprintf(&b, "add chain ip %s output { type nat hook output priority -100; policy accept; }\n", transparentTable)
	// Never redirect traffic already destined for loopback.
	fmt.Fprintf(&b, "add rule ip %s output ip daddr 127.0.0.0/8 return\n", transparentTable)
	// Redirect everyone but the MITM proxy itself. Two rules (80, 443) instead of
	// an anonymous set { 80, 443 } to keep the ruleset to exact-match dport loads.
	fmt.Fprintf(&b, "add rule ip %s output meta skuid != %d tcp dport 80 redirect to :%d\n", transparentTable, mitmUID, localPort)
	fmt.Fprintf(&b, "add rule ip %s output meta skuid != %d tcp dport 443 redirect to :%d\n", transparentTable, mitmUID, localPort)
	return b.String()
}

// SetupTransparentHTTP installs the native-nft transparent HTTP redirect:
// OUTPUT tcp dport 80,443 -> localPort for every UID except the MITM proxy's,
// with loopback excluded.
func SetupTransparentHTTP(localPort int, mitmUID uint32) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("nft transparent: only supported on linux")
	}
	if localPort <= 0 {
		return fmt.Errorf("nft transparent: invalid port")
	}
	log.Infof("installing nft transparent: OUTPUT tcp dport 80,443 -> 127.0.0.1:%d (skip uid %d)", localPort, mitmUID)
	if err := setupTransparent(context.Background(), defaultRunner, localPort, mitmUID); err != nil {
		return err
	}
	log.Infof("nft transparent rules installed successfully")
	return nil
}

func setupTransparent(ctx context.Context, run runner, localPort int, mitmUID uint32) error {
	if err := deleteTableIfPresent(ctx, run, "ip", transparentTable); err != nil {
		return err
	}
	script := transparentScript(localPort, mitmUID)
	if output, err := run(ctx, script); err != nil {
		return fmt.Errorf("nft transparent apply failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// RemoveTransparentHTTP deletes the transparent redirect table; a missing table
// is ignored.
func RemoveTransparentHTTP(localPort int, mitmUID uint32) {
	if runtime.GOOS != "linux" {
		return
	}
	if err := deleteTableIfPresent(context.Background(), defaultRunner, "ip", transparentTable); err != nil {
		log.Warnf("nft transparent remove (ignored): %v", err)
		return
	}
	log.Infof("nft transparent rules removed")
}
