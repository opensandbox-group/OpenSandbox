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
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// The native-nft twin of the transparent iptables rules (below): one table per family, an OUTPUT
// nat chain, loopback and the mitm uid's own connections returned, the intercepted ports
// redirected. `meta skuid` is nft-native — no `owner` xtables extension involved.
const mitmRedirectNftTable = "opensandbox_mitm_redirect"

func transparentHTTPRules(localPort int, mitmUID uint32, dports, op string) [][]string {
	target := strconv.Itoa(localPort)
	uid := strconv.FormatUint(uint64(mitmUID), 10)
	loopRules := [][]string{
		{"iptables", "-t", "nat", op, "OUTPUT", "-p", "tcp", "-d", "127.0.0.0/8", "-j", "RETURN"},
	}
	redir := [][]string{
		{
			"iptables", "-t", "nat", op, "OUTPUT", "-p", "tcp",
			"-m", "owner", "!", "--uid-owner", uid,
			"-m", "multiport", "--dports", dports,
			"-j", "REDIRECT", "--to-ports", target,
		},
	}
	return append(loopRules, redir...)
}

// transparentNftScript renders the same policy as transparentHTTPRules for nft, for BOTH address
// families (the iptables path only ever covered IPv4; on an IPv6-only network that is the whole
// traffic). Idempotent: the tables are deleted first (a missing table is retried without the delete).
func transparentNftScript(localPort int, mitmUID uint32, dports string) string {
	ports := strings.Join(strings.Split(dports, ","), ", ")
	uid := strconv.FormatUint(uint64(mitmUID), 10)
	var b strings.Builder
	for _, family := range []string{"ip", "ip6"} {
		fmt.Fprintf(&b, "delete table %s %s\n", family, mitmRedirectNftTable)
	}
	for _, family := range []string{"ip", "ip6"} {
		fmt.Fprintf(&b, "add table %s %s\n", family, mitmRedirectNftTable)
		fmt.Fprintf(&b, "add chain %s %s output { type nat hook output priority -100; policy accept; }\n", family, mitmRedirectNftTable)
		if family == "ip" {
			fmt.Fprintf(&b, "add rule ip %s output ip daddr 127.0.0.0/8 tcp dport { %s } return\n", mitmRedirectNftTable, ports)
		} else {
			fmt.Fprintf(&b, "add rule ip6 %s output ip6 daddr ::1 tcp dport { %s } return\n", mitmRedirectNftTable, ports)
		}
		fmt.Fprintf(&b, "add rule %s %s output meta skuid %s tcp dport { %s } return\n", family, mitmRedirectNftTable, uid, ports)
		fmt.Fprintf(&b, "add rule %s %s output tcp dport { %s } redirect to :%d\n", family, mitmRedirectNftTable, ports, localPort)
	}
	return b.String()
}

func setupTransparentNft(ctx context.Context, r redirectRunner, localPort int, mitmUID uint32, dports string) error {
	script := transparentNftScript(localPort, mitmUID, dports)
	output, err := r.runNft(ctx, script)
	if err != nil && isNftMissingTableErrorFor(output, err, mitmRedirectNftTable) {
		output, err = r.runNft(ctx, removeNftDeleteTableLineFor(script, mitmRedirectNftTable))
	}
	if err != nil {
		return fmt.Errorf("nft transparent redirect failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	log.Infof("nft transparent redirect installed: OUTPUT tcp dport %s -> :%d (skip uid %d), ip + ip6", dports, localPort, mitmUID)
	return nil
}

func removeTransparentNft(ctx context.Context, r redirectRunner) {
	for _, family := range []string{"ip", "ip6"} {
		script := fmt.Sprintf("delete table %s %s\n", family, mitmRedirectNftTable)
		if output, err := r.runNft(ctx, script); err != nil && !isNftMissingTableErrorFor(output, err, mitmRedirectNftTable) && !isNftUnavailableError(err) {
			log.Warnf("nft transparent redirect remove table (ignored): %v (output: %s)", err, strings.TrimSpace(string(output)))
		}
	}
}

// SetupTransparentHTTP: non-mitm UIDs get OUTPUT tcp:<dports> → localPort; loopback and mitm’s traffic excluded.
// dports is a validated iptables `--dports` list (e.g. "80,443" or "80,443,8080").
//
// Backend: OPENSANDBOX_EGRESS_REDIRECT_BACKEND (backend.go) — iptables first and a native nft
// fallback when iptables' nft backend cannot load the `owner`/`REDIRECT` xtables extensions
// (auto, the default), or nft from the start.
func SetupTransparentHTTP(localPort int, mitmUID uint32, dports string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("iptables transparent: only supported on linux")
	}

	if localPort <= 0 {
		return fmt.Errorf("iptables transparent: invalid port or uid")
	}
	if strings.TrimSpace(dports) == "" {
		return fmt.Errorf("iptables transparent: empty dports")
	}
	target := strconv.Itoa(localPort)
	uid := strconv.FormatUint(uint64(mitmUID), 10)
	ctx := context.Background()
	r := defaultRedirectRunner()
	if redirectBackend() == backendNft {
		return setupTransparentNft(ctx, r, localPort, mitmUID, dports)
	}
	log.Infof("installing iptables transparent: OUTPUT tcp dport %s -> 127.0.0.1:%s (skip uid %s)", dports, target, uid)

	rules := transparentHTTPRules(localPort, mitmUID, dports, "-A")
	var applied [][]string
	for _, args := range rules {
		if output, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			wrapped := fmt.Errorf("iptables transparent: %v (output: %s)", err, output)
			if redirectBackend() == backendIptables || !isIptablesXtExtensionError(wrapped) {
				return wrapped
			}
			// The kernel has nf_tables but not the xtables extension the rule needs: undo what
			// was appended and express the same rules natively.
			log.Warnf("iptables transparent failed in nft backend; falling back to native nft redirect: %v", wrapped)
			for i := len(applied) - 1; i >= 0; i-- {
				del := append([]string{}, applied[i]...)
				del[3] = "-D"
				_, _ = exec.Command(del[0], del[1:]...).CombinedOutput()
			}
			return setupTransparentNft(ctx, r, localPort, mitmUID, dports)
		}
		applied = append(applied, args)
	}
	log.Infof("iptables transparent rules installed successfully")
	return nil
}

func RemoveTransparentHTTP(localPort int, mitmUID uint32, dports string) {
	if runtime.GOOS != "linux" {
		return
	}
	if localPort <= 0 || strings.TrimSpace(dports) == "" {
		return
	}
	removeTransparentNft(context.Background(), defaultRedirectRunner())
	if redirectBackend() == backendNft {
		return
	}
	rules := transparentHTTPRules(localPort, mitmUID, dports, "-D")
	for i := len(rules) - 1; i >= 0; i-- {
		args := rules[i]
		if output, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			log.Warnf("iptables transparent remove rule (ignored): %v (output: %s)", err, strings.TrimSpace(string(output)))
		}
	}
	log.Infof("iptables transparent rules removed")
}
