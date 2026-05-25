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
	"os"
	"sync"
	"time"
)

type alwaysRuleFileState struct {
	path    string
	action  string
	exists  bool
	modTime time.Time
	size    int64
	rules   []EgressRule
}

// AlwaysRuleLoader polls the standard always-deny/allow file paths at most once per refreshInterval.
type AlwaysRuleLoader struct {
	mu              sync.RWMutex
	refreshInterval time.Duration
	lastCheck       time.Time
	denyState       alwaysRuleFileState
	allowState      alwaysRuleFileState
}

func NewAlwaysRuleLoader(refreshInterval time.Duration) *AlwaysRuleLoader {
	return newAlwaysRuleLoader(refreshInterval, alwaysDenyFilePath, alwaysAllowFilePath)
}

func newAlwaysRuleLoader(refreshInterval time.Duration, denyPath, allowPath string) *AlwaysRuleLoader {
	if refreshInterval <= 0 {
		refreshInterval = time.Minute
	}
	return &AlwaysRuleLoader{
		refreshInterval: refreshInterval,
		denyState:       alwaysRuleFileState{path: denyPath, action: ActionDeny},
		allowState:      alwaysRuleFileState{path: allowPath, action: ActionAllow},
	}
}

// RefreshIfDue reloads from disk when the interval elapsed; changed is true only if file content actually differed.
func (l *AlwaysRuleLoader) RefreshIfDue(now time.Time) (deny, allow []EgressRule, changed bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.lastCheck.IsZero() && now.Sub(l.lastCheck) < l.refreshInterval {
		return cloneRules(l.denyState.rules), cloneRules(l.allowState.rules), false, nil
	}
	l.lastCheck = now

	denyChanged, err := l.refreshOne(&l.denyState)
	if err != nil {
		return nil, nil, false, err
	}
	allowChanged, err := l.refreshOne(&l.allowState)
	if err != nil {
		return nil, nil, false, err
	}
	changed = denyChanged || allowChanged
	return cloneRules(l.denyState.rules), cloneRules(l.allowState.rules), changed, nil
}

func (l *AlwaysRuleLoader) CurrentRules() (deny, allow []EgressRule) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return cloneRules(l.denyState.rules), cloneRules(l.allowState.rules)
}

func (l *AlwaysRuleLoader) SetCurrentRules(deny, allow []EgressRule) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.denyState.rules = cloneRules(deny)
	l.allowState.rules = cloneRules(allow)
}

func (l *AlwaysRuleLoader) refreshOne(state *alwaysRuleFileState) (bool, error) {
	info, err := os.Stat(state.path)
	if err != nil {
		if os.IsNotExist(err) {
			if !state.exists {
				return false, nil
			}
			state.exists = false
			state.modTime = time.Time{}
			state.size = 0
			state.rules = nil
			return true, nil
		}
		return false, err
	}

	if state.exists && info.ModTime().Equal(state.modTime) && info.Size() == state.size {
		return false, nil
	}

	rules, err := loadAlwaysRuleFile(state.path, state.action)
	if err != nil {
		return false, err
	}
	state.exists = true
	state.modTime = info.ModTime()
	state.size = info.Size()
	state.rules = rules
	return true, nil
}

func cloneRules(in []EgressRule) []EgressRule {
	if len(in) == 0 {
		return nil
	}
	out := make([]EgressRule, len(in))
	copy(out, in)
	return out
}
