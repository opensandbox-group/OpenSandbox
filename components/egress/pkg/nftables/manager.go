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
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

const (
	tableName                        = "opensandbox"
	chainName                        = "egress"
	allowV4Set                       = "allow_v4"
	allowV6Set                       = "allow_v6"
	denyV4Set                        = "deny_v4"
	denyV6Set                        = "deny_v6"
	dohBlockV4Set                    = "doh_block_v4"
	dohBlockV6Set                    = "doh_block_v6"
	defaultConnectionRefreshInterval = 30 * time.Second
)

type runner func(ctx context.Context, script string) ([]byte, error)

type Options struct {
	BlockDoT       bool
	BlockDoH443    bool
	DoHBlocklistV4 []string
	DoHBlocklistV6 []string
	// ConnectionRefreshInterval controls how often active TCP connections renew
	// DNS-derived nft leases. Shorter intervals reduce the maximum temporary
	// reconnect gap, but increase /proc scans and nft updates. The 30-second
	// default is half the minimum 60-second DNS lease.
	ConnectionRefreshInterval time.Duration
}

type Manager struct {
	run     runner
	opts    Options
	mu      sync.Mutex
	tracker *connectionTracker
}

func NewManager() *Manager {
	return newManager(defaultRunner, Options{BlockDoT: true})
}

func NewManagerWithRunner(r runner) *Manager {
	return newManager(r, Options{BlockDoT: true})
}

func NewManagerWithRunnerAndOptions(r runner, opts Options) *Manager {
	return newManager(r, opts)
}

func NewManagerWithOptions(opts Options) *Manager {
	return newManager(defaultRunner, opts)
}

func newManager(r runner, opts Options) *Manager {
	if opts.ConnectionRefreshInterval <= 0 {
		opts.ConnectionRefreshInterval = defaultConnectionRefreshInterval
	}
	return &Manager{
		run:     r,
		opts:    opts,
		tracker: newConnectionTracker(),
	}
}

func (m *Manager) ApplyStatic(ctx context.Context, p *policy.NetworkPolicy) error {
	if p == nil {
		p = policy.DefaultDenyPolicy()
	}
	allowV4, allowV6, denyV4, denyV6 := p.StaticIPSets()
	log.Infof("nftables: applying static policy: default=%s, allow_v4=%d, allow_v6=%d, deny_v4=%d, deny_v6=%d",
		p.DefaultAction, len(allowV4), len(allowV6), len(denyV4), len(denyV6))
	m.mu.Lock()
	defer m.mu.Unlock()
	script, err := buildRuleset(p, m.opts)
	if err != nil {
		return err
	}
	if _, err := m.run(ctx, script); err != nil {
		if isMissingTableError(err) {
			fallback := removeDeleteTableLine(script)
			if fallback != script {
				if _, retryErr := m.run(ctx, fallback); retryErr == nil {
					m.tracker.clear()
					telemetry.SetNftablesRuleCount(telemetry.NftRuleCountFromPolicy(p))
					telemetry.RecordNftablesUpdate()
					return nil
				}
			}
		}
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpStaticApply)
		return err
	}
	m.tracker.clear()
	telemetry.SetNftablesRuleCount(telemetry.NftRuleCountFromPolicy(p))
	telemetry.RecordNftablesUpdate()
	log.Infof("nftables: static policy applied successfully")
	return nil
}

func (m *Manager) AddResolvedIPs(ctx context.Context, ips []ResolvedIP) error {
	if len(ips) == 0 {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	script := buildAddResolvedIPsScript(tableName, ips)
	if script == "" {
		return nil
	}
	log.Debugf("nftables: adding %d resolved IP(s) to dynamic allow sets with script statement %s", len(ips), script)
	_, err := m.run(ctx, script)
	if err != nil {
		// The policy allows these destinations but the kernel does not know it yet, so
		// the chain's final rule drops them. Indistinguishable from a policy denial
		// inside the sandbox, hence its own counter.
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpDynamicAdd)
		return err
	}
	m.tracker.setDynamicIPs(ips)
	telemetry.RecordNftablesUpdate()
	return nil
}

// StartConnectionRefresh keeps DNS-learned IPs authorized while a TCP
// connection to them is active. The normal set timeout remains as the grace
// period after the connection closes.
//
// Renewal is intentionally best-effort:
//   - an active connection first observed after its entry expires is restored
//     on the next poll, so reconnects may fail for up to one refresh interval;
//   - a connection that starts and closes entirely between polls cannot be
//     observed and requires a later DNS lookup to restore its expired entry;
//   - delayed polls or nft failures can extend the temporary reconnect gap;
//   - only TCP is tracked here; UDP and QUIC rely on DNS-driven entry refresh.
//
// Existing connections survive these gaps through conntrack. A successful
// observation renews the entry for the full timeout, and the final observation
// after close provides the same bounded grace period for reconnects.
func (m *Manager) StartConnectionRefresh(ctx context.Context) {
	m.tracker.start(ctx, m.opts.ConnectionRefreshInterval, m)
}

// RemoveEnforcement drops inet opensandbox; missing table is not an error.
func (m *Manager) RemoveEnforcement(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	script := fmt.Sprintf("delete table inet %s\n", tableName)
	_, err := m.run(ctx, script)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such file") || strings.Contains(msg, "does not exist") {
			return nil
		}
		telemetry.RecordNftablesUpdateFailed(telemetry.NftOpRemove)
		return err
	}
	m.tracker.clear()
	log.Infof("nftables: removed table inet %s", tableName)
	return nil
}

func buildRuleset(p *policy.NetworkPolicy, opts Options) (string, error) {
	allowV4, allowV6, denyV4, denyV6 := p.StaticIPSets()
	var err error
	if allowV4, err = normalizeNFTIntervalSet(allowV4); err != nil {
		return "", err
	}
	if allowV6, err = normalizeNFTIntervalSet(allowV6); err != nil {
		return "", err
	}
	if denyV4, err = normalizeNFTIntervalSet(denyV4); err != nil {
		return "", err
	}
	if denyV6, err = normalizeNFTIntervalSet(denyV6); err != nil {
		return "", err
	}
	dohBlockV4 := opts.DoHBlocklistV4
	dohBlockV6 := opts.DoHBlocklistV6
	if len(dohBlockV4) > 0 {
		if dohBlockV4, err = normalizeNFTIntervalSet(dohBlockV4); err != nil {
			return "", err
		}
	}
	if len(dohBlockV6) > 0 {
		if dohBlockV6, err = normalizeNFTIntervalSet(dohBlockV6); err != nil {
			return "", err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "delete table inet %s\n", tableName)
	fmt.Fprintf(&b, "add table inet %s\n", tableName)

	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", tableName, allowV4Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", tableName, denyV4Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", tableName, allowV6Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", tableName, denyV6Set)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; timeout %ds; }\n", tableName, dynAllowV4Set, dynSetTimeoutS)
	fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; timeout %ds; }\n", tableName, dynAllowV6Set, dynSetTimeoutS)

	if len(dohBlockV4) > 0 {
		fmt.Fprintf(&b, "add set inet %s %s { type ipv4_addr; flags interval; }\n", tableName, dohBlockV4Set)
	}
	if len(dohBlockV6) > 0 {
		fmt.Fprintf(&b, "add set inet %s %s { type ipv6_addr; flags interval; }\n", tableName, dohBlockV6Set)
	}

	writeElements(&b, allowV4Set, allowV4)
	writeElements(&b, denyV4Set, denyV4)
	writeElements(&b, allowV6Set, allowV6)
	writeElements(&b, denyV6Set, denyV6)
	writeElements(&b, dohBlockV4Set, dohBlockV4)
	writeElements(&b, dohBlockV6Set, dohBlockV6)

	chainPolicy := "drop"
	if p.DefaultAction == policy.ActionAllow {
		chainPolicy = "accept"
	}
	fmt.Fprintf(&b, "add chain inet %s %s { type filter hook output priority 0; policy %s; }\n", tableName, chainName, chainPolicy)
	fmt.Fprintf(&b, "add rule inet %s %s ct state established,related accept\n", tableName, chainName)
	fmt.Fprintf(&b, "add rule inet %s %s meta mark %s accept\n", tableName, chainName, constants.MarkHex)
	fmt.Fprintf(&b, "add rule inet %s %s oifname \"lo\" accept\n", tableName, chainName)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr 127.0.0.1 udp dport 15353 accept\n", tableName, chainName)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr 127.0.0.1 tcp dport 15353 accept\n", tableName, chainName)
	if opts.BlockDoT {
		fmt.Fprintf(&b, "add rule inet %s %s tcp dport 853 drop\n", tableName, chainName)
		fmt.Fprintf(&b, "add rule inet %s %s udp dport 853 drop\n", tableName, chainName)
	}
	if opts.BlockDoH443 {
		if len(dohBlockV4) == 0 && len(dohBlockV6) == 0 {
			// strict: drop all 443 when enabled but no blocklist provided
			fmt.Fprintf(&b, "add rule inet %s %s tcp dport 443 drop\n", tableName, chainName)
		} else {
			if len(dohBlockV4) > 0 {
				fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s tcp dport 443 drop\n", tableName, chainName, dohBlockV4Set)
			}
			if len(dohBlockV6) > 0 {
				fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s tcp dport 443 drop\n", tableName, chainName, dohBlockV6Set)
			}
		}
	}
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s drop\n", tableName, chainName, denyV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s drop\n", tableName, chainName, denyV6Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s accept\n", tableName, chainName, dynAllowV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s accept\n", tableName, chainName, dynAllowV6Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip daddr @%s accept\n", tableName, chainName, allowV4Set)
	fmt.Fprintf(&b, "add rule inet %s %s ip6 daddr @%s accept\n", tableName, chainName, allowV6Set)
	if chainPolicy == "drop" {
		fmt.Fprintf(&b, "add rule inet %s %s drop\n", tableName, chainName)
	}

	return b.String(), nil
}

func writeElements(b *strings.Builder, setName string, elems []string) {
	if len(elems) == 0 {
		return
	}
	fmt.Fprintf(b, "add element inet %s %s { %s }\n", tableName, setName, strings.Join(elems, ", "))
}

func defaultRunner(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("nft apply failed: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func isMissingTableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such file or directory") && strings.Contains(msg, "delete table inet "+tableName)
}

func removeDeleteTableLine(script string) string {
	lines := strings.Split(script, "\n")
	var filtered []string
	for _, l := range lines {
		if strings.HasPrefix(l, "delete table inet "+tableName) {
			continue
		}
		if strings.TrimSpace(l) == "" {
			continue
		}
		filtered = append(filtered, l)
	}
	return strings.Join(filtered, "\n")
}
