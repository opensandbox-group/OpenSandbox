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

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/policy"
	slogger "github.com/alibaba/opensandbox/internal/logger"
)

const maxPolicyBodyBytes = 1 << 20

func readPolicyRequestBody(r *http.Request) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBodyBytes))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

func patchMergedPolicy(base *policy.NetworkPolicy, patchRules []policy.EgressRule) (*policy.NetworkPolicy, error) {
	if base == nil {
		base = policy.DefaultDenyPolicy()
	}
	baseCopy := *base
	baseCopy.Egress = append([]policy.EgressRule(nil), base.Egress...)

	merged := mergeEgressRules(baseCopy.Egress, patchRules)
	rawMerged, err := json.Marshal(policy.NetworkPolicy{
		DefaultAction: baseCopy.DefaultAction,
		Egress:        merged,
	})
	if err != nil {
		return nil, err
	}
	return policy.ParsePolicy(string(rawMerged))
}

func mergeEgressRules(base, additions []policy.EgressRule) []policy.EgressRule {
	if len(additions) == 0 {
		return base
	}
	out := make([]policy.EgressRule, 0, len(base)+len(additions))
	seen := make(map[string]struct{})

	// patch rules win on same target; base fills the rest
	for _, r := range additions {
		key := mergeKey(r)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	for _, r := range base {
		key := mergeKey(r)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// mergeKey: domain targets lowercased for dedupe; IP/CIDR left as-is.
func mergeKey(r policy.EgressRule) string {
	if r.Target == "" {
		return r.Target
	}
	return strings.ToLower(r.Target)
}

func maxEgressRulesFromEnv() int {
	s := strings.TrimSpace(os.Getenv(constants.EnvMaxEgressRules))
	if s == "" {
		return constants.DefaultMaxEgressRules
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return constants.DefaultMaxEgressRules
	}
	if n == 0 {
		return 0
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func modeFromPolicy(p *policy.NetworkPolicy) string {
	if p == nil {
		return "deny_all"
	}
	if p.DefaultAction == policy.ActionAllow && len(p.Egress) == 0 {
		return "allow_all"
	} else if p.DefaultAction == policy.ActionDeny && len(p.Egress) == 0 {
		return "deny_all"
	}

	return "enforcing"
}

func policyRuleSummary(p *policy.NetworkPolicy) []map[string]string {
	if p == nil {
		return nil
	}
	return egressRulesSummary(p.Egress)
}

func egressRulesSummary(egress []policy.EgressRule) []map[string]string {
	out := make([]map[string]string, 0, len(egress))
	for _, r := range egress {
		out = append(out, map[string]string{
			"action": r.Action,
			"target": r.Target,
		})
	}
	return out
}

func logEgressLoaded(pol *policy.NetworkPolicy) {
	if pol == nil {
		pol = policy.DefaultDenyPolicy()
	}
	fields := []slogger.Field{
		{Key: "opensandbox.event", Value: "egress.loaded"},
		{Key: "egress.default", Value: pol.DefaultAction},
		{Key: "rules", Value: policyRuleSummary(pol)},
	}
	log.Logger.With(fields...).Infof("egress policy loaded")
}

// logEgressUpdated: egress.updated event. rules is only the delta for this request (PATCH: patch list;
// POST/PUT: full body egress; reset: empty), defaultAction is the policy after apply.
func logEgressUpdated(defaultAction string, deltaEgress []policy.EgressRule) {
	fields := []slogger.Field{
		{Key: "opensandbox.event", Value: "egress.updated"},
		{Key: "egress.default", Value: defaultAction},
		{Key: "rules", Value: egressRulesSummary(deltaEgress)},
	}
	log.Logger.With(fields...).Infof("egress policy updated")
}

func logEgressUpdateFailedWarn(msg string) {
	fields := []slogger.Field{
		{Key: "opensandbox.event", Value: "egress.update_failed"},
		{Key: "error", Value: msg},
	}
	log.Logger.With(fields...).Warnf("egress policy update failed")
}

func logEgressUpdateFailedError(msg string) {
	fields := []slogger.Field{
		{Key: "opensandbox.event", Value: "egress.update_failed"},
		{Key: "error", Value: msg},
	}
	log.Logger.With(fields...).Errorf("egress policy update failed")
}
