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

package main

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/fastsandboxnft"
	"github.com/alibaba/opensandbox/egress/pkg/subject"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// transitionRuleModel interprets cumulative add-rule, flush-chain, and
// delete-table effects from the real production Applier. This is a narrow
// script-order model, not a kernel/netns or live packet-isolation test.
type transitionRuleModel struct {
	scripts []string
	rules   []string
}

func (m *transitionRuleModel) run(_ context.Context, script string) ([]byte, error) {
	m.scripts = append(m.scripts, script)
	for _, line := range strings.Split(script, "\n") {
		fields := strings.Fields(line)
		switch {
		case line == "delete table inet "+fastsandboxnft.TableName:
			m.rules = nil
		case len(fields) == 5 && fields[0] == "flush" && fields[1] == "chain":
			prefix := "add rule " + strings.Join(fields[2:], " ") + " "
			retained := m.rules[:0]
			for _, r := range m.rules {
				if !strings.HasPrefix(r, prefix) {
					retained = append(retained, r)
				}
			}
			m.rules = retained
		case strings.HasPrefix(line, "add rule inet "+fastsandboxnft.TableName+" "):
			m.rules = append(m.rules, line)
		}
	}
	return nil, nil
}

func TestTransitionChangedIPRebuildRemovesCumulativeOldDispatch(t *testing.T) {
	for _, unknownAfterPartialInstall := range []bool{false, true} {
		name := "active-old"
		if unknownAfterPartialInstall {
			name = "unknown-old-network-after-partial-install"
		}
		t.Run(name, func(t *testing.T) {
			srv, reg, _ := fastSandboxTestServer(t)
			model := &transitionRuleModel{}
			srv.nft = fastsandboxnft.NewApplier(model.run, fastsandboxnft.Options{MitmRedirectPort: 18080})
			subj := subject.FromSandboxUID(testUID)
			if unknownAfterPartialInstall {
				srv.dnsRedirectApply = func(context.Context, []netip.Addr, int) error {
					return errors.New("gateway redirect failure after nft install")
				}
			}
			initial := doAction(t, srv, bindBody(t, testUID, testIP, testFenceR, testFenceA, 1, vaultAllowPolicy()))
			if unknownAfterPartialInstall {
				require.Equal(t, http.StatusInternalServerError, initial.Code)
				// Model older failure metadata: actual nft rules exist, successful
				// attachment bookkeeping does not. Unknown must take Remove path.
				srv.mu.Lock()
				delete(srv.subjAtt, subj)
				srv.mu.Unlock()
				srv.dnsRedirectApply = func(context.Context, []netip.Addr, int) error { return nil }
			} else {
				require.Equal(t, http.StatusOK, initial.Code)
				require.Equal(t, http.StatusOK, doAction(t, srv, hookBody(t, testUID, testFenceR, testFenceA, constants.HookDataPlaneReady)).Code)
				require.Equal(t, subject.StateActive, mustState(reg.Get(subj)))
			}
			unrelatedPolicy := `{"defaultAction":"allow"}`
			unrelated := setBindingAndReady(t, srv, reg, "unrelated", "10.0.0.9", &unrelatedPolicy)
			unrelatedRules := func() []string {
				var rules []string
				for _, rule := range model.rules {
					if strings.Contains(rule, "s_unrelated") {
						rules = append(rules, rule)
					}
				}
				return rules
			}
			beforeUnrelated := unrelatedRules()
			require.Contains(t, strings.Join(beforeUnrelated, "\n"), "meta mark set 0x2")
			oldRules := strings.Join(model.rules, "\n")
			for _, chain := range []string{"dispatch", "marking", "input"} {
				require.Contains(t, oldRules, chain+" ip saddr "+testIP)
			}
			before := len(model.scripts)
			require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, testUID, "10.0.0.6", "B", "B-att", 2, vaultAllowPolicy())).Code)
			assert.GreaterOrEqual(t, len(model.scripts)-before, 2, "Remove rebuild must precede the new deny-first install")
			assert.Contains(t, model.scripts[before], "delete table inet "+fastsandboxnft.TableName)
			assert.NotContains(t, model.scripts[before], "ip saddr "+testIP)
			if len(model.scripts) > before+1 {
				assert.Contains(t, model.scripts[before+1], "ip saddr 10.0.0.6")
			}
			require.ElementsMatch(t, beforeUnrelated, unrelatedRules(), "full rebuild must preserve unrelated active policy and dispatch")
			require.Equal(t, subject.StateActive, mustState(reg.Get(unrelated)))
			require.Equal(t, http.StatusOK, doAction(t, srv, bindBody(t, "successor", testIP, "C", "C-att", 1, vaultAllowPolicy())).Code)
			owner, ok := reg.Resolve(subject.SubjectKey{SourceIP: netip.MustParseAddr(testIP)})
			require.True(t, ok)
			require.Equal(t, subject.FromSandboxUID("successor"), owner)
			require.Equal(t, subject.StateDenying, mustState(reg.Get(owner)))
			require.ElementsMatch(t, beforeUnrelated, unrelatedRules())
			for _, chain := range []string{"dispatch", "marking", "input"} {
				var matches []string
				for _, rule := range model.rules {
					if strings.Contains(rule, chain+" ip saddr "+testIP+" ") && strings.Contains(rule, " jump ") {
						matches = append(matches, rule)
					}
				}
				require.Len(t, matches, 1, "cumulative %s rules must not retain old owner at reused IP", chain)
				require.Contains(t, matches[0], "s_successor")
				require.NotContains(t, matches[0], "s_u_1")
			}
		})
	}
}
