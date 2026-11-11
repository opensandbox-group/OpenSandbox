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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMergeAlwaysOverlay_OrderAndPrecedence(t *testing.T) {
	user, err := ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"allow","target":"evil.com"}]}`)
	require.NoError(t, err)

	deny, err := ParseValidatedEgressRule(ActionDeny, "evil.com")
	require.NoError(t, err)
	merged := MergeAlwaysOverlay(user, []EgressRule{deny}, nil)
	require.Equal(t, ActionDeny, merged.Evaluate("evil.com."), "always deny must override user allow")

	user2, err := ParsePolicy(`{"defaultAction":"deny","egress":[{"action":"deny","target":"good.com"}]}`)
	require.NoError(t, err)
	allow, err := ParseValidatedEgressRule(ActionAllow, "good.com")
	require.NoError(t, err)
	merged2 := MergeAlwaysOverlay(user2, nil, []EgressRule{allow})
	require.Equal(t, ActionAllow, merged2.Evaluate("good.com."), "always allow must override user deny")
}

func TestMergeAlwaysOverlay_DenyAlwaysBeatsAllowAlways(t *testing.T) {
	user := DefaultDenyPolicy()
	deny, err := ParseValidatedEgressRule(ActionDeny, "x.com")
	require.NoError(t, err)
	allow, err := ParseValidatedEgressRule(ActionAllow, "x.com")
	require.NoError(t, err)
	merged := MergeAlwaysOverlay(user, []EgressRule{deny}, []EgressRule{allow})
	require.Equal(t, ActionDeny, merged.Evaluate("x.com."))
}

func TestParseAlwaysRuleLines(t *testing.T) {
	raw := "# c\n\n192.0.2.1\n2001:db8::/32\n*.foo.test\n"
	got, err := parseAlwaysRuleLines([]byte(raw), ActionDeny, "test")
	require.NoError(t, err)
	require.Len(t, got, 3)
}

func TestLoadAlwaysRuleFile_Missing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope")
	got, err := loadAlwaysRuleFile(path, ActionDeny)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestParseValidatedEgressRule_EmptyTarget(t *testing.T) {
	_, err := ParseValidatedEgressRule(ActionDeny, "")
	require.Error(t, err)
}

func TestAlwaysRuleLoader_RefreshIntervalAndReloadByMTime(t *testing.T) {
	dir := t.TempDir()
	denyPath := filepath.Join(dir, "deny.always")
	allowPath := filepath.Join(dir, "allow.always")
	require.NoError(t, os.WriteFile(denyPath, []byte("1.1.1.1\n"), 0o644))

	loader := newAlwaysRuleLoader(time.Minute, denyPath, allowPath, filepath.Join(filepath.Dir(denyPath), "ports.always"))
	t0 := time.Unix(1000, 0)

	deny, allow, changed, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, deny, 1)
	require.Nil(t, allow)
	require.Equal(t, "1.1.1.1", deny[0].Target)

	require.NoError(t, os.WriteFile(denyPath, []byte("2.2.2.2\n"), 0o644))
	require.NoError(t, os.Chtimes(denyPath, t0.Add(10*time.Second), t0.Add(10*time.Second)))
	deny, _, changed, err = loader.RefreshIfDue(t0.Add(30 * time.Second))
	require.NoError(t, err)
	require.False(t, changed, "should skip checks before refresh interval")
	require.Len(t, deny, 1)
	require.Equal(t, "1.1.1.1", deny[0].Target, "cached rules should remain before interval")

	deny, _, changed, err = loader.RefreshIfDue(t0.Add(61 * time.Second))
	require.NoError(t, err)
	require.True(t, changed, "mtime changed after interval, should reload")
	require.Len(t, deny, 1)
	require.Equal(t, "2.2.2.2", deny[0].Target)
}

func TestAlwaysRuleLoader_DeleteFileRemovesRules(t *testing.T) {
	dir := t.TempDir()
	denyPath := filepath.Join(dir, "deny.always")
	allowPath := filepath.Join(dir, "allow.always")
	require.NoError(t, os.WriteFile(denyPath, []byte("3.3.3.3\n"), 0o644))

	loader := newAlwaysRuleLoader(time.Minute, denyPath, allowPath, filepath.Join(filepath.Dir(denyPath), "ports.always"))
	t0 := time.Unix(2000, 0)

	deny, _, changed, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, deny, 1)

	require.NoError(t, os.Remove(denyPath))
	deny, _, changed, err = loader.RefreshIfDue(t0.Add(61 * time.Second))
	require.NoError(t, err)
	require.True(t, changed, "file deletion should be treated as rules removed")
	require.Nil(t, deny)
}

func TestAlwaysRuleLoader_ParseFailureDoesNotPartiallyPublishEitherFile(t *testing.T) {
	dir := t.TempDir()
	denyPath := filepath.Join(dir, "deny.always")
	allowPath := filepath.Join(dir, "allow.always")
	require.NoError(t, os.WriteFile(denyPath, []byte("1.1.1.1\n"), 0o644))
	require.NoError(t, os.WriteFile(allowPath, []byte("2.2.2.2\n"), 0o644))
	loader := newAlwaysRuleLoader(time.Minute, denyPath, allowPath, filepath.Join(filepath.Dir(denyPath), "ports.always"))
	t0 := time.Unix(3000, 0)
	oldDeny, oldAllow, changed, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)
	require.True(t, changed)

	require.NoError(t, os.WriteFile(denyPath, []byte("3.3.3.3\n"), 0o644))
	require.NoError(t, os.Chtimes(denyPath, t0.Add(10*time.Second), t0.Add(10*time.Second)))
	require.NoError(t, os.WriteFile(allowPath, []byte(strings.Repeat("a", 70*1024)), 0o644))
	require.NoError(t, os.Chtimes(allowPath, t0.Add(10*time.Second), t0.Add(10*time.Second)))

	_, _, _, err = loader.RefreshIfDue(t0.Add(61 * time.Second))
	require.Error(t, err)
	deny, allow := loader.CurrentRules()
	require.Equal(t, oldDeny, deny, "a failed pair parse must preserve the active deny rules")
	require.Equal(t, oldAllow, allow, "a failed pair parse must preserve the active allow rules")
}

func TestAlwaysRuleLoader_ParseFailureCanRetryBeforeInterval(t *testing.T) {
	dir := t.TempDir()
	denyPath := filepath.Join(dir, "deny.always")
	allowPath := filepath.Join(dir, "allow.always")
	require.NoError(t, os.WriteFile(denyPath, []byte("1.1.1.1\n"), 0o644))
	require.NoError(t, os.WriteFile(allowPath, []byte("2.2.2.2\n"), 0o644))
	loader := newAlwaysRuleLoader(time.Minute, denyPath, allowPath, filepath.Join(filepath.Dir(denyPath), "ports.always"))
	t0 := time.Unix(4000, 0)
	_, _, _, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(denyPath, []byte("3.3.3.3\n"), 0o644))
	require.NoError(t, os.Chtimes(denyPath, t0.Add(10*time.Second), t0.Add(10*time.Second)))
	require.NoError(t, os.WriteFile(allowPath, []byte(strings.Repeat("a", 70*1024)), 0o644))
	require.NoError(t, os.Chtimes(allowPath, t0.Add(10*time.Second), t0.Add(10*time.Second)))
	_, _, _, err = loader.RefreshIfDue(t0.Add(61 * time.Second))
	require.Error(t, err)

	require.NoError(t, os.WriteFile(allowPath, []byte("4.4.4.4\n"), 0o644))
	require.NoError(t, os.Chtimes(allowPath, t0.Add(11*time.Second), t0.Add(11*time.Second)))
	deny, allow, changed, err := loader.RefreshIfDue(t0.Add(62 * time.Second))
	require.NoError(t, err, "a parse failure must not suppress a retry before the next interval")
	require.True(t, changed)
	require.Equal(t, "3.3.3.3", deny[0].Target)
	require.Equal(t, "4.4.4.4", allow[0].Target)
}

func TestAlwaysRuleLoader_ApplyFailurePreservesRulesAndCanRetry(t *testing.T) {
	dir := t.TempDir()
	denyPath := filepath.Join(dir, "deny.always")
	allowPath := filepath.Join(dir, "allow.always")
	require.NoError(t, os.WriteFile(denyPath, []byte("1.1.1.1\n"), 0o644))
	require.NoError(t, os.WriteFile(allowPath, []byte("2.2.2.2\n"), 0o644))
	loader := newAlwaysRuleLoader(time.Minute, denyPath, allowPath, filepath.Join(filepath.Dir(denyPath), "ports.always"))
	t0 := time.Unix(5000, 0)
	oldDeny, oldAllow, changed, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)
	require.True(t, changed)

	require.NoError(t, os.WriteFile(denyPath, []byte("3.3.3.3\n"), 0o644))
	require.NoError(t, os.Chtimes(denyPath, t0.Add(10*time.Second), t0.Add(10*time.Second)))
	applyFailure := errors.New("external nft apply failed")
	_, _, changed, err = loader.RefreshIfDueWithApply(t0.Add(61*time.Second), func(deny, _ []EgressRule) error {
		require.Equal(t, "3.3.3.3", deny[0].Target)
		deny[0].Target = "mutated by callback"
		return applyFailure
	})
	require.ErrorIs(t, err, applyFailure)
	require.False(t, changed)
	deny, allow := loader.CurrentRules()
	require.Equal(t, oldDeny, deny, "failed apply must preserve current deny rules")
	require.Equal(t, oldAllow, allow, "failed apply must preserve current allow rules")

	deny, allow, changed, err = loader.RefreshIfDueWithApply(t0.Add(62*time.Second), func(deny, _ []EgressRule) error {
		require.Equal(t, "3.3.3.3", deny[0].Target, "retry must receive a fresh candidate")
		return nil
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "3.3.3.3", deny[0].Target)
	require.Equal(t, oldAllow, allow)
}

func TestParseAllowedPortRanges(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []PortRange
		wantErr bool
	}{
		{
			name:    "single ports and ranges, unsorted and overlapping",
			content: "8443\n443\n1-79\n80\n8000-9000\n# comment\n\n",
			want:    []PortRange{{Lo: 1, Hi: 80}, {Lo: 443, Hi: 443}, {Lo: 8000, Hi: 9000}},
		},
		{name: "adjacent ranges coalesce", content: "1-10\n11-20\n", want: []PortRange{{Lo: 1, Hi: 20}}},
		{name: "blank file", content: "# nothing\n", want: nil},
		{name: "zero port rejected", content: "0\n", wantErr: true},
		{name: "overflow port rejected", content: "65536\n", wantErr: true},
		{name: "inverted range rejected", content: "100-90\n", wantErr: true},
		{name: "garbage rejected", content: "https\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAllowedPortRanges([]byte(tt.content), "test")
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestComplementRanges(t *testing.T) {
	require.Equal(t, []PortRange{{Lo: 1, Hi: 65535}}, complementRanges(nil))
	got := complementRanges([]PortRange{{Lo: 80, Hi: 80}, {Lo: 443, Hi: 443}, {Lo: 8443, Hi: 8443}})
	require.Equal(t, []PortRange{
		{Lo: 1, Hi: 79}, {Lo: 81, Hi: 442}, {Lo: 444, Hi: 8442}, {Lo: 8444, Hi: 65535},
	}, got)
	// Boundaries covered: allow everything except the very first and last ports.
	got = complementRanges([]PortRange{{Lo: 1, Hi: 1}, {Lo: 65535, Hi: 65535}})
	require.Equal(t, []PortRange{{Lo: 2, Hi: 65534}}, got)
	// Allow everything: no complement.
	require.Nil(t, complementRanges([]PortRange{{Lo: 1, Hi: 65535}}))
}

func TestLoadPortFloorRules(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	got, err := loadPortFloorRules(missing)
	require.NoError(t, err)
	require.Nil(t, got, "missing file means no floor")

	empty := filepath.Join(dir, "empty.always")
	require.NoError(t, os.WriteFile(empty, []byte("# only a comment\n"), 0o644))
	got, err = loadPortFloorRules(empty)
	require.NoError(t, err)
	require.Nil(t, got, "an empty whitelist must not deny every port by accident")

	floor := filepath.Join(dir, "reachable.ports.cap")
	require.NoError(t, os.WriteFile(floor, []byte("80\n443\n"), 0o644))
	got, err = loadPortFloorRules(floor)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, ActionDeny, got[0].Action)
	require.Empty(t, got[0].Target)
	require.Equal(t, []PortRange{{Lo: 1, Hi: 79}, {Lo: 81, Hi: 442}, {Lo: 444, Hi: 65535}}, got[0].PortRanges)
}

func TestAlwaysRuleLoader_PortsFloorHotReload(t *testing.T) {
	dir := t.TempDir()
	denyPath := filepath.Join(dir, "deny.always")
	portsPath := filepath.Join(dir, "reachable.ports.cap")
	require.NoError(t, os.WriteFile(denyPath, []byte("1.1.1.1\n"), 0o644))
	require.NoError(t, os.WriteFile(portsPath, []byte("443\n"), 0o644))

	loader := newAlwaysRuleLoader(time.Minute, denyPath, reachablePortsFilePath_unusedForTest(dir), portsPath)
	t0 := time.Unix(1000, 0)
	deny, _, changed, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)
	require.True(t, changed)
	// deny.always rule first, floor rule appended after it.
	require.Len(t, deny, 2)
	require.Equal(t, "1.1.1.1", deny[0].Target)
	require.Equal(t, []PortRange{{Lo: 1, Hi: 442}, {Lo: 444, Hi: 65535}}, deny[1].PortRanges)

	// Editing only the ports file must hot-reload the floor (mtime of
	// deny.always untouched).
	require.NoError(t, os.WriteFile(portsPath, []byte("443\n8443\n"), 0o644))
	require.NoError(t, os.Chtimes(portsPath, t0.Add(10*time.Second), t0.Add(10*time.Second)))
	deny, _, changed, err = loader.RefreshIfDue(t0.Add(61 * time.Second))
	require.NoError(t, err)
	require.True(t, changed, "ports file change alone must trigger a reload")
	require.Len(t, deny, 2)
	require.Equal(t, []PortRange{{Lo: 1, Hi: 442}, {Lo: 444, Hi: 8442}, {Lo: 8444, Hi: 65535}}, deny[1].PortRanges)

	// Deleting the ports file removes the floor.
	require.NoError(t, os.Remove(portsPath))
	require.NoError(t, os.Chtimes(denyPath, t0.Add(20*time.Second), t0.Add(20*time.Second)))
	deny, _, changed, err = loader.RefreshIfDue(t0.Add(122 * time.Second))
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, deny, 1, "floor must disappear with its file")
	require.Equal(t, "1.1.1.1", deny[0].Target)
}

// reachablePortsFilePath_unusedForTest keeps the deny/allow pair positional in
// test setup without touching the real /var/egress paths.
func reachablePortsFilePath_unusedForTest(dir string) string {
	return filepath.Join(dir, "allow.always")
}

func TestAlwaysRuleLoader_SetCurrentRulesDoesNotDuplicateFloor(t *testing.T) {
	dir := t.TempDir()
	portsPath := filepath.Join(dir, "reachable.ports.cap")
	require.NoError(t, os.WriteFile(portsPath, []byte("443\n"), 0o644))
	loader := newAlwaysRuleLoader(time.Minute,
		filepath.Join(dir, "deny.always"),
		filepath.Join(dir, "allow.always"),
		portsPath)
	t0 := time.Unix(1000, 0)
	deny, _, _, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)
	require.Len(t, deny, 1)

	// Recovery-style reseed with the loader's own output (floor included in
	// deny): the ports-owned floor must not be duplicated.
	loader.SetCurrentRules(deny, nil)
	current, _ := loader.CurrentRules()
	require.Len(t, current, 1, "floor seeded back through SetCurrentRules must be stripped")

	// And the next refresh must not re-append on top of the seeded copy.
	fresh, _, changed, err := loader.RefreshIfDue(t0.Add(61 * time.Second))
	require.NoError(t, err)
	require.False(t, changed, "nothing changed on disk")
	require.Len(t, fresh, 1)
	require.Equal(t, []PortRange{{Lo: 1, Hi: 442}, {Lo: 444, Hi: 65535}}, fresh[0].PortRanges)
}

func TestWarnPortAllowRulesBlockedByCap(t *testing.T) {
	// floor for cap {80, 443}: everything except 80/443 is dropped.
	floor := []EgressRule{NewPortFloorDenyRule([]PortRange{{Lo: 1, Hi: 79}, {Lo: 81, Hi: 442}, {Lo: 444, Hi: 65535}})}

	fullyBlocked := []EgressRule{{Action: ActionAllow, Ports: []int{22}}}
	partiallyBlocked := []EgressRule{{Action: ActionAllow, Ports: []int{80, 9090}}}
	inside := []EgressRule{{Action: ActionAllow, Ports: []int{80}}}
	denies := []EgressRule{{Action: ActionDeny, Ports: []int{22}}}

	// The pure predicate drives the warning; assert its truth table.
	require.True(t, portAllowRuleFullyBlocked(fullyBlocked[0], portFloorDenyRangesForTest(floor)))
	require.False(t, portAllowRuleFullyBlocked(partiallyBlocked[0], portFloorDenyRangesForTest(floor)))
	require.False(t, portAllowRuleFullyBlocked(inside[0], portFloorDenyRangesForTest(floor)))
	require.False(t, portAllowRuleFullyBlocked(denies[0], portFloorDenyRangesForTest(floor)))
	require.False(t, portAllowRuleFullyBlocked(EgressRule{Action: ActionAllow}, portFloorDenyRangesForTest(floor)))

	// MergeAlwaysOverlay must not mutate the user policy and must tolerate a
	// cap-less overlay (no floor rule -> no check).
	merged := MergeAlwaysOverlay(&NetworkPolicy{DefaultAction: ActionDeny, Egress: fullyBlocked}, floor, nil)
	require.Len(t, merged.Egress, 2)
	merged = MergeAlwaysOverlay(&NetworkPolicy{DefaultAction: ActionDeny, Egress: fullyBlocked}, nil, nil)
	require.Len(t, merged.Egress, 1)
}

func portFloorDenyRangesForTest(floor []EgressRule) []PortRange {
	return floor[0].PortRanges
}

func TestAlwaysRuleLoader_FloorSurvivesPolicyApplyRace(t *testing.T) {
	// SET_BINDING replay / PATCH racing startup: SetCurrentRules re-seeds the
	// ports state with the floor it strips from the deny input, so the
	// published view never drops the platform floor.
	dir := t.TempDir()
	portsPath := filepath.Join(dir, "reachable.ports.cap")
	require.NoError(t, os.WriteFile(portsPath, []byte("443\n"), 0o644))
	loader := newAlwaysRuleLoader(time.Minute,
		filepath.Join(dir, "deny.always"),
		filepath.Join(dir, "allow.always"),
		portsPath)
	t0 := time.Unix(1000, 0)
	deny, _, _, err := loader.RefreshIfDue(t0)
	require.NoError(t, err)
	require.Len(t, deny, 1)

	loader.SetCurrentRules(deny, nil)
	current, _ := loader.CurrentRules()
	require.Len(t, current, 1, "floor must stay published immediately after a reseed")
	require.Equal(t, []PortRange{{Lo: 1, Hi: 442}, {Lo: 444, Hi: 65535}}, current[0].PortRanges)
}
