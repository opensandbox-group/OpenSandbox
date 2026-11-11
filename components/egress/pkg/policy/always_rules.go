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

package policy

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// Fixed paths for operator-managed lists. Missing files are ignored (no effect).
const (
	alwaysDenyFilePath  = "/var/egress/rules/deny.always"
	alwaysAllowFilePath = "/var/egress/rules/allow.always"
	// reachablePortsFilePath is the platform TCP port cap: whitelist lines
	// enforced as their complement, ahead of every sandbox rule.
	reachablePortsFilePath = "/var/egress/rules/reachable.ports.cap"
)

// LoadAlwaysRuleFiles loads optional /var/egress/rules/deny|allow.always (ignore if missing); errors on unreadable.
// The reachable.ports.cap floor rides inside the deny slice so every
// consumer of the overlay enforces it uniformly.
func LoadAlwaysRuleFiles() (deny, allow []EgressRule, err error) {
	deny, err = loadAlwaysRuleFile(alwaysDenyFilePath, ActionDeny)
	if err != nil {
		return nil, nil, err
	}
	allow, err = loadAlwaysRuleFile(alwaysAllowFilePath, ActionAllow)
	if err != nil {
		return nil, nil, err
	}
	portFloor, err := loadPortFloorRules(reachablePortsFilePath)
	if err != nil {
		return nil, nil, err
	}
	log.Infof("loaded %d always-deny rule(s) from %s", len(deny), alwaysDenyFilePath)
	log.Infof("loaded %d always-allow rule(s) from %s", len(allow), alwaysAllowFilePath)
	deny = append(deny, portFloor...)
	return deny, allow, nil
}

func loadAlwaysRuleFileWithAction(action string) func(path string) ([]EgressRule, error) {
	return func(path string) ([]EgressRule, error) {
		return loadAlwaysRuleFile(path, action)
	}
}

// LoadReachablePortsCap reads reachable.ports.cap (whitelist lines: "80",
// "443", "8000-9000") and returns the merged allowed ranges; missing file
// yields nil. Exported for the fast-sandbox profile.
func LoadReachablePortsCap() ([]PortRange, error) {
	data, err := os.ReadFile(reachablePortsFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseAllowedPortRanges(data, reachablePortsFilePath)
}

// loadPortFloorRules returns reachable.ports.cap's complement as a single
// always-deny rule; a missing or empty file yields nil (an accidentally
// empty whitelist must not deny every connection).
func loadPortFloorRules(path string) ([]EgressRule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	allowed, err := parseAllowedPortRanges(data, path)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		log.Infof("%s declares no ports; no port floor enforced", path)
		return nil, nil
	}
	denyRanges := complementRanges(allowed)
	if len(denyRanges) == 0 {
		return nil, nil
	}
	rule := EgressRule{
		Action:     ActionDeny,
		PortRanges: denyRanges,
		targetKind: targetPortOnly,
	}
	log.Infof("loaded port floor from %s: %d allowed range(s), %d denied range(s)",
		path, len(allowed), len(denyRanges))
	return []EgressRule{rule}, nil
}

// NewPortFloorDenyRule builds the port-floor deny rule for callers that
// already hold complement ranges (tests).
func NewPortFloorDenyRule(denyRanges []PortRange) EgressRule {
	return EgressRule{
		Action:     ActionDeny,
		PortRanges: denyRanges,
		targetKind: targetPortOnly,
	}
}

// DenyRangesForAllowedPorts returns 1-65535 minus the allowed ranges
// (merged, sorted) — the drop intervals for a port floor.
func DenyRangesForAllowedPorts(allowed []PortRange) []PortRange {
	return complementRanges(allowed)
}

// parseAllowedPortRanges parses whitelist lines into merged, sorted,
// non-overlapping ranges; '#' comments and blanks are ignored.
func parseAllowedPortRanges(data []byte, pathForErr string) ([]PortRange, error) {
	var out []PortRange
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := parsePortRangeToken(line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", pathForErr, lineNum, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", pathForErr, err)
	}
	return mergeRanges(out), nil
}

// parsePortRangeToken parses "80" or "lo-hi" (inclusive, 1-65535, lo<=hi).
func parsePortRangeToken(token string) (PortRange, error) {
	loStr, hiStr, bound := strings.Cut(token, "-")
	lo, err := strconv.Atoi(strings.TrimSpace(loStr))
	if err != nil {
		return PortRange{}, fmt.Errorf("invalid port %q", token)
	}
	hi := lo
	if bound {
		hi, err = strconv.Atoi(strings.TrimSpace(hiStr))
		if err != nil {
			return PortRange{}, fmt.Errorf("invalid port range %q", token)
		}
	}
	if lo < 1 || hi > 65535 || lo > hi {
		return PortRange{}, fmt.Errorf("port range %q out of 1-65535 or inverted", token)
	}
	return PortRange{Lo: lo, Hi: hi}, nil
}

// mergeRanges sorts and coalesces overlapping or adjacent ranges.
func mergeRanges(ranges []PortRange) []PortRange {
	if len(ranges) == 0 {
		return nil
	}
	sorted := append([]PortRange(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Lo < sorted[j].Lo })
	merged := []PortRange{sorted[0]}
	for _, r := range sorted[1:] {
		last := &merged[len(merged)-1]
		if r.Lo <= last.Hi+1 {
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

// complementRanges returns 1-65535 minus the allowed ranges, merged and
// sorted. Allowed input must already be merged (see mergeRanges).
func complementRanges(allowed []PortRange) []PortRange {
	var out []PortRange
	next := 1
	for _, r := range allowed {
		if r.Lo > next {
			out = append(out, PortRange{Lo: next, Hi: r.Lo - 1})
		}
		if r.Hi >= next {
			next = r.Hi + 1
		}
	}
	if next <= 65535 {
		out = append(out, PortRange{Lo: next, Hi: 65535})
	}
	return out
}

func loadAlwaysRuleFile(path, action string) ([]EgressRule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseAlwaysRuleLines(data, action, path)
}

func parseAlwaysRuleLines(data []byte, action, pathForErr string) ([]EgressRule, error) {
	var out []EgressRule
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	lineNum := 0
	for sc.Scan() {
		lineNum++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule, err := ParseValidatedEgressRule(action, line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", pathForErr, lineNum, err)
		}
		out = append(out, rule)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", pathForErr, err)
	}
	return out, nil
}

func ParseValidatedEgressRule(action, target string) (EgressRule, error) {
	p := NetworkPolicy{
		DefaultAction: ActionDeny,
		Egress:        []EgressRule{{Action: action, Target: target}},
	}
	if err := normalizePolicy(&p); err != nil {
		return EgressRule{}, err
	}
	return p.Egress[0], nil
}

// MergeAlwaysOverlay prepends always-deny, then always-allow, then user egress.
// First matching domain rule in Evaluate wins; deny.always therefore overrides
// user rules and allow.always for the same target. Between two always files,
// deny entries are ordered before allow entries so deny wins on duplicate targets.
func MergeAlwaysOverlay(user *NetworkPolicy, alwaysDeny, alwaysAllow []EgressRule) *NetworkPolicy {
	if user == nil {
		user = DefaultDenyPolicy()
	}
	out := *user
	out.Egress = append([]EgressRule(nil), user.Egress...)
	n := len(alwaysDeny) + len(alwaysAllow) + len(out.Egress)
	merged := make([]EgressRule, 0, n)
	merged = append(merged, alwaysDeny...)
	merged = append(merged, alwaysAllow...)
	merged = append(merged, out.Egress...)
	out.Egress = merged
	warnPortAllowRulesBlockedByCap(user.Egress, alwaysDeny)
	return ensureDefaults(&out)
}

// warnPortAllowRulesBlockedByCap flags user port-allow rules whose ports
// are entirely covered by the cap (the rule is stored but can never pass).
// Partially blocked rules are not logged.
func warnPortAllowRulesBlockedByCap(user []EgressRule, alwaysDeny []EgressRule) {
	var capDenyRanges []PortRange
	for _, r := range alwaysDeny {
		if len(r.PortRanges) > 0 {
			capDenyRanges = append(capDenyRanges, r.PortRanges...)
		}
	}
	if len(capDenyRanges) == 0 {
		return
	}
	for _, r := range user {
		if !portAllowRuleFullyBlocked(r, capDenyRanges) {
			continue
		}
		log.Warnf("port allow rule is fully blocked by the reachable-ports cap: target=%q ports=%v — every port it opens is dropped by the platform cap; remove the rule or ask the operator to extend reachable.ports.cap",
			r.Target, r.Ports)
	}
}

// portAllowRuleFullyBlocked reports whether r is an allow rule whose ports
// are all covered by the cap's deny ranges.
func portAllowRuleFullyBlocked(r EgressRule, capDenyRanges []PortRange) bool {
	if r.Action != ActionAllow || len(r.Ports) == 0 {
		return false
	}
	for _, port := range r.Ports {
		blocked := false
		for _, rg := range capDenyRanges {
			if rg.Contains(port) {
				blocked = true
				break
			}
		}
		if !blocked {
			return false
		}
	}
	return true
}
