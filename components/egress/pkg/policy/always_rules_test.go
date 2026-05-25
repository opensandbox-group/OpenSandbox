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
	"path/filepath"
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

	loader := newAlwaysRuleLoader(time.Minute, denyPath, allowPath)
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

	loader := newAlwaysRuleLoader(time.Minute, denyPath, allowPath)
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
