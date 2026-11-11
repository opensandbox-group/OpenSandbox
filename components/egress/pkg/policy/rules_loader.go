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
	"os"
	"sync"
	"time"
)

type alwaysRuleFileState struct {
	path    string
	action  string
	load    func(path string) ([]EgressRule, error)
	exists  bool
	modTime time.Time
	size    int64
	rules   []EgressRule
}

// AlwaysRuleLoader polls the standard always-deny/allow file paths (plus the
// reachable.ports.cap floor) at most once per refreshInterval.
type AlwaysRuleLoader struct {
	mu              sync.RWMutex
	refreshInterval time.Duration
	lastCheck       time.Time
	denyState       alwaysRuleFileState
	allowState      alwaysRuleFileState
	// portsState owns the floor rules: appended to the published deny set,
	// and stripped from SetCurrentRules input so reseeding never duplicates.
	portsState alwaysRuleFileState
}

func NewAlwaysRuleLoader(refreshInterval time.Duration) *AlwaysRuleLoader {
	return newAlwaysRuleLoader(refreshInterval, alwaysDenyFilePath, alwaysAllowFilePath, reachablePortsFilePath)
}

func newAlwaysRuleLoader(refreshInterval time.Duration, denyPath, allowPath, portsPath string) *AlwaysRuleLoader {
	if refreshInterval <= 0 {
		refreshInterval = time.Minute
	}
	return &AlwaysRuleLoader{
		refreshInterval: refreshInterval,
		denyState:       alwaysRuleFileState{path: denyPath, action: ActionDeny, load: loadAlwaysRuleFileWithAction(ActionDeny)},
		allowState:      alwaysRuleFileState{path: allowPath, action: ActionAllow, load: loadAlwaysRuleFileWithAction(ActionAllow)},
		portsState:      alwaysRuleFileState{path: portsPath, load: loadPortFloorRules},
	}
}

// RefreshIfDue reloads from disk when the interval elapsed; changed is true only if file content actually differed.
func (l *AlwaysRuleLoader) RefreshIfDue(now time.Time) (deny, allow []EgressRule, changed bool, err error) {
	return l.RefreshIfDueWithApply(now, nil)
}

// RefreshIfDueWithApply stages all three files and invokes apply with an isolated candidate before publishing it.
// Parse or apply errors leave the active rules, file metadata, and refresh deadline unchanged so the same
// candidate can be retried immediately.
func (l *AlwaysRuleLoader) RefreshIfDueWithApply(
	now time.Time,
	apply func(deny, allow []EgressRule) error,
) (deny, allow []EgressRule, changed bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.lastCheck.IsZero() && now.Sub(l.lastCheck) < l.refreshInterval {
		deny, allow := l.publishedRules()
		return deny, allow, false, nil
	}

	nextDenyState, denyChanged, err := refreshOneCandidate(l.denyState)
	if err != nil {
		return nil, nil, false, err
	}
	nextAllowState, allowChanged, err := refreshOneCandidate(l.allowState)
	if err != nil {
		return nil, nil, false, err
	}
	nextPortsState, portsChanged, err := refreshOneCandidate(l.portsState)
	if err != nil {
		return nil, nil, false, err
	}
	changed = denyChanged || allowChanged || portsChanged
	if changed && apply != nil {
		candidateDeny := append(cloneRules(nextDenyState.rules), cloneRules(nextPortsState.rules)...)
		if err := apply(candidateDeny, cloneRules(nextAllowState.rules)); err != nil {
			return nil, nil, false, err
		}
	}
	l.denyState = nextDenyState
	l.allowState = nextAllowState
	l.portsState = nextPortsState
	l.lastCheck = now
	deny, allow = l.publishedRules()
	return deny, allow, changed, nil
}

// publishedRules is the active view: deny.always rules plus the derived port
// floor from the ports state.
func (l *AlwaysRuleLoader) publishedRules() (deny, allow []EgressRule) {
	return append(cloneRules(l.denyState.rules), cloneRules(l.portsState.rules)...), cloneRules(l.allowState.rules)
}

func (l *AlwaysRuleLoader) CurrentRules() (deny, allow []EgressRule) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return l.publishedRules()
}

func (l *AlwaysRuleLoader) SetCurrentRules(deny, allow []EgressRule) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Re-seed the floor here, or a reseed racing startup would briefly
	// publish without it.
	l.portsState.rules = cloneRulesKeepingPortRanges(deny)
	l.denyState.rules = cloneRulesWithoutPortRanges(deny)
	l.allowState.rules = cloneRules(allow)
}

func refreshOneCandidate(state alwaysRuleFileState) (alwaysRuleFileState, bool, error) {
	info, err := os.Stat(state.path)
	if err != nil {
		if os.IsNotExist(err) {
			if !state.exists {
				return state, false, nil
			}
			next := state
			next.exists = false
			next.modTime = time.Time{}
			next.size = 0
			next.rules = nil
			return next, true, nil
		}
		return state, false, err
	}

	if state.exists && info.ModTime().Equal(state.modTime) && info.Size() == state.size {
		return state, false, nil
	}

	rules, err := state.load(state.path)
	if err != nil {
		return state, false, err
	}
	next := state
	next.exists = true
	next.modTime = info.ModTime()
	next.size = info.Size()
	next.rules = cloneRules(rules)
	return next, true, nil
}

func cloneRules(in []EgressRule) []EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]EgressRule, len(in))
	copy(out, in)
	return out
}

// cloneRulesWithoutPortRanges drops floor rules (owned by the ports state).
func cloneRulesWithoutPortRanges(in []EgressRule) []EgressRule {
	out := cloneRules(in)
	kept := out[:0]
	for _, r := range out {
		if len(r.PortRanges) == 0 {
			kept = append(kept, r)
		}
	}
	return kept
}

// cloneRulesKeepingPortRanges keeps only floor rules (seeds the ports state).
func cloneRulesKeepingPortRanges(in []EgressRule) []EgressRule {
	var out []EgressRule
	for _, r := range cloneRules(in) {
		if len(r.PortRanges) > 0 {
			out = append(out, r)
		}
	}
	return out
}
