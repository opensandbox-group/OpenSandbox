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

package policy

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// Fixed paths for operator-managed lists. Missing files are ignored (no effect).
const (
	alwaysDenyFilePath  = "/var/egress/rules/deny.always"
	alwaysAllowFilePath = "/var/egress/rules/allow.always"
)

// LoadAlwaysRuleFiles loads optional /var/egress/rules/deny|allow.always (ignore if missing); errors on unreadable.
func LoadAlwaysRuleFiles() (deny, allow []EgressRule, err error) {
	deny, err = loadAlwaysRuleFile(alwaysDenyFilePath, ActionDeny)
	if err != nil {
		return nil, nil, err
	}
	allow, err = loadAlwaysRuleFile(alwaysAllowFilePath, ActionAllow)
	if err != nil {
		return nil, nil, err
	}
	log.Infof("loaded %d always-deny rule(s) from %s", len(deny), alwaysDenyFilePath)
	log.Infof("loaded %d always-allow rule(s) from %s", len(allow), alwaysAllowFilePath)
	return deny, allow, nil
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
	return ensureDefaults(&out)
}
