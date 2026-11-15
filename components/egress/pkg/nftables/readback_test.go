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

package nftables

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The fixture table mirrors the quarantine fence's shape (three filter base
// chains with unconditional drop rules) under a different table name and
// owner, so the tests prove the verifier is table-scoped and not hard-coded
// to the fence.
func testTableJSON() []byte {
	return []byte(`{"nftables":[
 {"metainfo":{"version":"1.0.9","json_schema_version":1}},
 {"table":{"family":"inet","name":"opensandbox_test","handle":1,"comment":"opensandbox test v1"}},
 {"chain":{"family":"inet","table":"opensandbox_test","name":"input","handle":2,"type":"filter","hook":"input","prio":-450,"policy":"drop"}},
 {"chain":{"family":"inet","table":"opensandbox_test","name":"output","handle":3,"type":"filter","hook":"output","prio":-450,"policy":"drop"}},
 {"chain":{"family":"inet","table":"opensandbox_test","name":"forward","handle":4,"type":"filter","hook":"forward","prio":-450,"policy":"drop"}},
 {"rule":{"family":"inet","table":"opensandbox_test","chain":"input","handle":5,"expr":[{"drop":null}]}},
 {"rule":{"family":"inet","table":"opensandbox_test","chain":"output","handle":6,"expr":[{"drop":null}]}},
 {"rule":{"family":"inet","table":"opensandbox_test","chain":"forward","handle":7,"expr":[{"drop":null}]}}
 ]}`)
}

var absentTestTableJSON = []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":"inet","name":"opensandbox"}}]}`)

const testTableText = `table inet opensandbox_test {
    comment "opensandbox test v1"
    chain input {
        type filter hook input priority -450; policy drop;
        drop
    }
    chain output {
        type filter hook output priority -450; policy drop;
        drop
    }
    chain forward {
        type filter hook forward priority -450; policy drop;
        drop
    }
}
`

var testTableReadback = TableReadback{
	Family: "inet",
	Table:  "opensandbox_test",
	Owner:  "opensandbox test v1",
	Chains: map[string]ChainReadback{
		"input":   {Type: "filter", Hook: "input", Prio: json.Number("-450"), Policy: "drop"},
		"output":  {Type: "filter", Hook: "output", Prio: json.Number("-450"), Policy: "drop"},
		"forward": {Type: "filter", Hook: "forward", Prio: json.Number("-450"), Policy: "drop"},
	},
	Rules: map[string]RuleMatcher{
		"input":   UnconditionalDrop,
		"output":  UnconditionalDrop,
		"forward": UnconditionalDrop,
	},
	Text: testTableText,
}

func TestVerifyTableRequiresReadback(t *testing.T) {
	failed := errors.New("injected nft failure")
	for _, tc := range []struct {
		name             string
		json             []byte
		jsonErr, textErr error
		text             string
		cancelAfterJSON  bool
		wantPresent      bool
		wantErr          bool
		wantCalls        int
	}{
		{"present", testTableJSON(), nil, nil, testTableText, false, true, false, 2},
		{"absent", absentTestTableJSON, nil, nil, "", false, false, false, 1},
		{"foreign-table-only", []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":"inet","name":"opensandbox"}}]}`), nil, nil, "", false, false, false, 1},
		{"other-family-only", []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":"ip","name":"opensandbox_test","handle":1}}]}`), nil, nil, "", false, false, false, 1},
		{"json-list-failure", testTableJSON(), failed, nil, testTableText, false, false, true, 1},
		{"json-mismatch", []byte(strings.ReplaceAll(string(testTableJSON()), `"drop":null`, `"accept":null`)), nil, nil, testTableText, false, false, true, 1},
		{"malformed-json", []byte(`{"nftables":`), nil, nil, testTableText, false, false, true, 1},
		{"trailing-data", []byte(`{"nftables":[]} {}`), nil, nil, testTableText, false, false, true, 1},
		{"text-list-failure", testTableJSON(), nil, failed, testTableText, false, false, true, 2},
		{"text-mismatch", testTableJSON(), nil, nil, strings.Replace(testTableText, "policy drop", "policy accept", 1), false, false, true, 2},
		{"legacy-json-requires-owned-text", []byte(strings.Replace(string(testTableJSON()), `,"comment":"opensandbox test v1"`, "", 1)), nil, nil, testTableText, false, true, false, 2},
		{"legacy-json-wrong-owner-text", []byte(strings.Replace(string(testTableJSON()), `,"comment":"opensandbox test v1"`, "", 1)), nil, nil, strings.Replace(testTableText, "opensandbox test v1", "foreign", 1), false, false, true, 2},
		{"cancelled-before-text", testTableJSON(), nil, nil, testTableText, true, false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			run := func(runCtx context.Context, args ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					require.Equal(t, []string{"-j", "list", "ruleset", "inet"}, args)
					if tc.cancelAfterJSON {
						cancel()
					}
					return tc.json, tc.jsonErr
				}
				require.Equal(t, []string{"-n", "-y", "list", "table", "inet", "opensandbox_test"}, args)
				return []byte(tc.text), tc.textErr
			}
			present, err := VerifyTable(ctx, run, testTableReadback)
			require.Equal(t, tc.wantPresent, present)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.jsonErr != nil {
				require.ErrorIs(t, err, tc.jsonErr, "a failed list must surface its error, never read as absence")
			}
			if tc.textErr != nil {
				require.ErrorIs(t, err, tc.textErr)
			}
			if tc.cancelAfterJSON {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Equal(t, tc.wantCalls, calls)
		})
	}
}

func TestInspectTableJSONStrict(t *testing.T) {
	for _, tc := range []struct {
		name      string
		index     int
		kind, key string
		value     any
	}{
		{"null-owner", 1, "table", "comment", nil},
		{"wrong-owner", 1, "table", "comment", "foreign"},
		{"dormant-table", 1, "table", "flags", []string{"dormant"}},
		{"process-owned-table", 1, "table", "flags", []string{"owner", "persist"}},
		{"string-flags", 1, "table", "flags", "dormant"},
		{"unknown-table-property", 1, "table", "disabled", true},
		{"wrong-hook", 2, "chain", "hook", "output"},
		{"late-priority", 2, "chain", "prio", 0},
		{"symbolic-priority", 2, "chain", "prio", "raw"},
		{"accept-policy", 2, "chain", "policy", "accept"},
		{"nat-chain", 2, "chain", "type", "nat"},
		{"device-bound-chain", 2, "chain", "dev", "eth0"},
		{"conditional-drop", 5, "rule", "expr", []any{map[string]any{"match": map[string]any{"left": "meta mark", "right": 0}}, map[string]any{"drop": nil}}},
		{"accept-rule", 5, "rule", "expr", []any{map[string]any{"accept": nil}}},
		{"invalid-drop-value", 5, "rule", "expr", []any{map[string]any{"drop": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var doc struct {
				Nftables []map[string]any `json:"nftables"`
			}
			require.NoError(t, json.Unmarshal(testTableJSON(), &doc))
			doc.Nftables[tc.index][tc.kind].(map[string]any)[tc.key] = tc.value
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			present, err := InspectTableJSON(data, testTableReadback)
			require.Error(t, err)
			require.False(t, present)
		})
	}
	for _, index := range []int{1, 2, 3, 4, 5, 6, 7} {
		for _, duplicate := range []bool{false, true} {
			var doc struct {
				Nftables []map[string]any `json:"nftables"`
			}
			require.NoError(t, json.Unmarshal(testTableJSON(), &doc))
			if duplicate {
				doc.Nftables = append(doc.Nftables, doc.Nftables[index])
			} else {
				doc.Nftables = append(doc.Nftables[:index], doc.Nftables[index+1:]...)
			}
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			_, err = InspectTableJSON(data, testTableReadback)
			require.Error(t, err, "index=%d duplicate=%v", index, duplicate)
		}
	}
	for _, data := range [][]byte{nil, []byte(`{}`), []byte(`null`), []byte(`{"nftables":null}`), []byte(`{"nftables":[]} {}`), []byte(`{"nftables":[],"extra":true}`), []byte(`{"nftables":[null]}`), []byte(`{"nftables":[{"table":null}]}`), []byte(`{"nftables":[{"set":{"family":"inet","table":"opensandbox_test","name":"unexpected"}}]}`)} {
		_, err := InspectTableJSON(data, testTableReadback)
		require.Error(t, err, "%s", data)
	}
	present, err := InspectTableJSON(testTableJSON(), testTableReadback)
	require.NoError(t, err)
	require.True(t, present)
	present, err = InspectTableJSON(absentTestTableJSON, testTableReadback)
	require.NoError(t, err)
	require.False(t, present)
	present, err = InspectTableJSON([]byte(strings.Replace(string(testTableJSON()), `"handle":1`, `"handle":1,"flags":[]`, 1)), testTableReadback)
	require.NoError(t, err)
	require.True(t, present)
}

func TestVerifyTableTextStrict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"exact", testTableText, false},
		{"text-extra-object", strings.Replace(testTableText, "    chain input", "    set unexpected { type ipv4_addr; }\n    chain input", 1), true},
		{"text-extra-chain", strings.Replace(testTableText, "    chain input", "    chain extra { }\n    chain input", 1), true},
		{"text-extra-rule", strings.Replace(testTableText, "        drop", "        accept\n        drop", 1), true},
		{"text-wrong-hook", strings.Replace(testTableText, "hook input", "hook prerouting", 1), true},
		{"text-wrong-priority", strings.Replace(testTableText, "priority -450", "priority 0", 1), true},
		{"text-wrong-policy", strings.Replace(testTableText, "policy drop", "policy accept", 1), true},
		{"text-unclosed-table", strings.TrimSuffix(testTableText, "}\n"), true},
		{"text-trailing-content", testTableText + "table inet extra {}\n", true},
		{"text-owner-internal-space", strings.Replace(testTableText, "opensandbox test v1", "opensandbox  test v1", 1), true},
		{"text-rule-internal-space", strings.Replace(testTableText, "type filter", "type  filter", 1), true},
		{"text-outer-whitespace", "\n\t" + strings.ReplaceAll(testTableText, "\n", " \t\n\n\t"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					require.Equal(t, []string{"-j", "list", "ruleset", "inet"}, args)
					return testTableJSON(), nil
				}
				require.Equal(t, []string{"-n", "-y", "list", "table", "inet", "opensandbox_test"}, args)
				return []byte(tc.text), nil
			}
			present, err := VerifyTable(context.Background(), run, testTableReadback)
			if tc.wantErr {
				require.Error(t, err)
				require.False(t, present)
			} else {
				require.NoError(t, err)
				require.True(t, present)
			}
			require.Equal(t, 2, calls)
		})
	}
}

func TestInspectTableJSONNilMatcherIsAnError(t *testing.T) {
	spec := testTableReadback
	spec.Rules = map[string]RuleMatcher{"input": nil, "output": UnconditionalDrop, "forward": UnconditionalDrop}
	_, err := InspectTableJSON(testTableJSON(), spec)
	require.Error(t, err, "a nil rule matcher must fail verification, never panic")
}

func TestUnconditionalDrop(t *testing.T) {
	require.True(t, UnconditionalDrop([]any{map[string]any{"drop": nil}}))
	for _, expr := range []any{
		nil,
		[]any{},
		[]any{map[string]any{"accept": nil}},
		[]any{map[string]any{"drop": true}},
		[]any{map[string]any{"drop": nil}, map[string]any{"drop": nil}},
		[]any{map[string]any{"match": map[string]any{"left": "meta mark"}}, map[string]any{"drop": nil}},
		"drop",
	} {
		require.False(t, UnconditionalDrop(expr), "%v", expr)
	}
}
