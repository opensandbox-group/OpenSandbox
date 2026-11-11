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

package quarantine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func fenceJSON() []byte {
	return []byte(`{"nftables":[
 {"metainfo":{"version":"1.0.9","json_schema_version":1}},
 {"table":{"family":"inet","name":"opensandbox_quarantine","handle":1,"comment":"opensandbox quarantine v1"}},
 {"chain":{"family":"inet","table":"opensandbox_quarantine","name":"input","handle":2,"type":"filter","hook":"input","prio":-450,"policy":"drop"}},
 {"chain":{"family":"inet","table":"opensandbox_quarantine","name":"output","handle":3,"type":"filter","hook":"output","prio":-450,"policy":"drop"}},
 {"chain":{"family":"inet","table":"opensandbox_quarantine","name":"forward","handle":4,"type":"filter","hook":"forward","prio":-450,"policy":"drop"}},
 {"rule":{"family":"inet","table":"opensandbox_quarantine","chain":"input","handle":5,"expr":[{"drop":null}]}},
 {"rule":{"family":"inet","table":"opensandbox_quarantine","chain":"output","handle":6,"expr":[{"drop":null}]}},
 {"rule":{"family":"inet","table":"opensandbox_quarantine","chain":"forward","handle":7,"expr":[{"drop":null}]}}
 ]}`)
}

var absentFenceJSON = []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":"inet","name":"opensandbox"}}]}`)

func TestFenceEnsureRequiresReadback(t *testing.T) {
	failed := errors.New("injected nft failure")
	for _, tc := range []struct {
		name              string
		writeErr, readErr error
		readback          []byte
		wantErr           bool
	}{
		{"committed", nil, nil, fenceJSON(), false},
		{"failed-before-apply", failed, nil, absentFenceJSON, true},
		{"failed-before-apply-already-fenced", failed, nil, fenceJSON(), false},
		{"failed-after-apply", failed, nil, fenceJSON(), false},
		{"successful-command-without-effect", nil, nil, absentFenceJSON, true},
		{"readback-failure-after-apply", nil, failed, fenceJSON(), true},
		{"readback-failure-after-uncertain-apply", failed, failed, fenceJSON(), true},
		{"readback-mismatch-after-apply", nil, nil, []byte(strings.ReplaceAll(string(fenceJSON()), `"drop":null`, `"accept":null`)), true},
		{"stderr-does-not-prove-safety", errors.New("No such file or directory"), nil, absentFenceJSON, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f := &Fence{run: func(_ context.Context, script string, args ...string) ([]byte, error) {
				calls++
				if calls == 1 {
					require.Equal(t, fenceInstallScript, script)
					require.Equal(t, []string{"-f", "-"}, args)
					return nil, tc.writeErr
				}
				require.Empty(t, script)
				if calls == 2 {
					require.Equal(t, []string{"-j", "list", "ruleset", "inet"}, args)
					return tc.readback, tc.readErr
				}
				require.Equal(t, []string{"-n", "-y", "list", "table", "inet", fenceTable}, args)
				return []byte(fenceExpectedTable), nil
			}}
			err := f.Ensure(context.Background())
			if tc.wantErr {
				require.ErrorContains(t, err, "installation is unconfirmed")
				if tc.writeErr != nil {
					require.ErrorIs(t, err, tc.writeErr)
				}
			} else {
				require.NoError(t, err)
			}
			wantCalls := 2
			if present, err := inspectFence(tc.readback); tc.readErr == nil && err == nil && present {
				wantCalls = 3
			}
			require.Equal(t, wantCalls, calls, "both complete snapshots are required after a command, including a lost result")
		})
	}
}

func TestFenceReadbackStrict(t *testing.T) {
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
			require.NoError(t, json.Unmarshal(fenceJSON(), &doc))
			doc.Nftables[tc.index][tc.kind].(map[string]any)[tc.key] = tc.value
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			present, err := inspectFence(data)
			require.Error(t, err)
			require.False(t, present)
		})
	}
	for _, index := range []int{1, 2, 3, 4, 5, 6, 7} {
		for _, duplicate := range []bool{false, true} {
			var doc struct {
				Nftables []map[string]any `json:"nftables"`
			}
			require.NoError(t, json.Unmarshal(fenceJSON(), &doc))
			if duplicate {
				doc.Nftables = append(doc.Nftables, doc.Nftables[index])
			} else {
				doc.Nftables = append(doc.Nftables[:index], doc.Nftables[index+1:]...)
			}
			data, err := json.Marshal(doc)
			require.NoError(t, err)
			_, err = inspectFence(data)
			require.Error(t, err, "index=%d duplicate=%v", index, duplicate)
		}
	}
	for _, data := range [][]byte{nil, []byte(`{}`), []byte(`null`), []byte(`{"nftables":null}`), []byte(`{"nftables":[]} {}`), []byte(`{"nftables":[],"extra":true}`), []byte(`{"nftables":[null]}`), []byte(`{"nftables":[{"table":null}]}`), []byte(`{"nftables":[{"set":{"family":"inet","table":"opensandbox_quarantine","name":"unexpected"}}]}`)} {
		_, err := inspectFence(data)
		require.Error(t, err, "%s", data)
	}
	present, err := inspectFence(fenceJSON())
	require.NoError(t, err)
	require.True(t, present)
	present, err = inspectFence(absentFenceJSON)
	require.NoError(t, err)
	require.False(t, present)
	present, err = inspectFence([]byte(strings.Replace(string(fenceJSON()), `"handle":1`, `"handle":1,"flags":[]`, 1)))
	require.NoError(t, err)
	require.True(t, present)
}

func TestFenceCancellationAndSerialization(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &Fence{run: func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("canceled operation submitted nft")
		return nil, nil
	}}
	require.ErrorIs(t, f.Ensure(ctx), context.Canceled)
	ctx, cancel = context.WithCancel(context.Background())
	f = &Fence{run: func(_ context.Context, script string, _ ...string) ([]byte, error) {
		if script != "" {
			cancel()
		}
		return fenceJSON(), nil
	}}
	require.ErrorIs(t, f.Ensure(ctx), context.Canceled)
	started, release, queued := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var sequence []string
	f = &Fence{run: func(_ context.Context, script string, args ...string) ([]byte, error) {
		if args[0] == "-n" {
			sequence = append(sequence, "text")
			return []byte(fenceExpectedTable), nil
		}
		if script == fenceInstallScript {
			sequence = append(sequence, "ensure")
			if len(sequence) == 1 {
				close(started)
				<-release
			}
		} else {
			sequence = append(sequence, "read")
			return fenceJSON(), nil
		}
		return nil, nil
	}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); require.NoError(t, f.Ensure(context.Background())) }()
	<-started
	go func() { defer wg.Done(); close(queued); require.NoError(t, f.Ensure(context.Background())) }()
	<-queued
	close(release)
	wg.Wait()
	require.Equal(t, []string{"ensure", "read", "text", "ensure", "read", "text"}, sequence)
}

func TestFenceScriptIsAnIndependentAtomicBatch(t *testing.T) {
	require.True(t, strings.HasPrefix(fenceInstallScript, "add table inet opensandbox_quarantine\ndelete table inet opensandbox_quarantine\n"))
	require.NotContains(t, fenceInstallScript, "flush ruleset")
	require.NotContains(t, fenceInstallScript, "table inet opensandbox\n")
	for _, exception := range []string{"accept", "ct state", "meta mark", "skuid", "lo\"", "redirect"} {
		require.NotContains(t, fenceInstallScript, exception)
	}
	for _, hook := range []string{"input", "output", "forward"} {
		require.Contains(t, fenceInstallScript, "type filter hook "+hook+" priority -450; policy drop;")
	}
}

func TestFenceMissingBinaryCannotProveIsolation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	fence := NewFence()
	require.ErrorContains(t, fence.Ensure(context.Background()), "installation is unconfirmed")
}

func TestFenceRequiresCompleteTextSnapshot(t *testing.T) {
	legacyJSON := []byte(strings.Replace(string(fenceJSON()), `,"comment":"opensandbox quarantine v1"`, "", 1))
	failure := errors.New("injected text readback failure")
	for _, tc := range []struct {
		name                string
		json                []byte
		text                string
		writeErr, textErr   error
		cancelText, wantErr bool
	}{
		{name: "legacy-json-requires-owned-text", json: legacyJSON, text: fenceExpectedTable},
		{name: "lost-result-with-two-complete-snapshots", json: legacyJSON, text: fenceExpectedTable, writeErr: failure},
		{name: "text-read-failure", text: fenceExpectedTable, textErr: failure, wantErr: true},
		{name: "lost-result-and-text-failure", text: fenceExpectedTable, writeErr: failure, textErr: failure, wantErr: true},
		{name: "text-cancelled", text: fenceExpectedTable, cancelText: true, wantErr: true},
		{name: "legacy-json-without-text", json: legacyJSON, wantErr: true},
		{name: "text-missing-owner", json: legacyJSON, text: strings.Replace(fenceExpectedTable, `    comment "opensandbox quarantine v1"`+"\n", "", 1), wantErr: true},
		{name: "text-wrong-owner", json: legacyJSON, text: strings.Replace(fenceExpectedTable, fenceOwner, "foreign", 1), wantErr: true},
		{name: "json-omitted-flags-text-dormant", text: strings.Replace(fenceExpectedTable, "    comment", "    flags dormant\n    comment", 1), wantErr: true},
		{name: "json-omitted-flags-text-owner", text: strings.Replace(fenceExpectedTable, "    comment", "    flags owner\n    comment", 1), wantErr: true},
		{name: "text-extra-object", text: strings.Replace(fenceExpectedTable, "    chain input", "    set unexpected { type ipv4_addr; }\n    chain input", 1), wantErr: true},
		{name: "text-extra-chain", text: strings.Replace(fenceExpectedTable, "    chain input", "    chain extra { }\n    chain input", 1), wantErr: true},
		{name: "text-extra-rule", text: strings.Replace(fenceExpectedTable, "        drop", "        accept\n        drop", 1), wantErr: true},
		{name: "text-wrong-hook", text: strings.Replace(fenceExpectedTable, "hook input", "hook prerouting", 1), wantErr: true},
		{name: "text-wrong-priority", text: strings.Replace(fenceExpectedTable, "priority -450", "priority 0", 1), wantErr: true},
		{name: "text-wrong-policy", text: strings.Replace(fenceExpectedTable, "policy drop", "policy accept", 1), wantErr: true},
		{name: "text-unclosed-table", text: strings.TrimSuffix(fenceExpectedTable, "}\n"), wantErr: true},
		{name: "text-trailing-content", text: fenceExpectedTable + "table inet extra {}\n", wantErr: true},
		{name: "text-owner-internal-space", text: strings.Replace(fenceExpectedTable, "opensandbox quarantine v1", "opensandbox  quarantine v1", 1), wantErr: true},
		{name: "text-rule-internal-space", text: strings.Replace(fenceExpectedTable, "type filter", "type  filter", 1), wantErr: true},
		{name: "text-outer-whitespace", text: "\n\t" + strings.ReplaceAll(fenceExpectedTable, "\n", " \t\n\n\t")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			f := &Fence{run: func(_ context.Context, script string, args ...string) ([]byte, error) {
				calls++
				switch calls {
				case 1:
					require.Equal(t, fenceInstallScript, script)
					return nil, tc.writeErr
				case 2:
					require.Empty(t, script)
					require.Equal(t, []string{"-j", "list", "ruleset", "inet"}, args)
					if tc.json != nil {
						return tc.json, nil
					}
					return fenceJSON(), nil
				case 3:
					require.Empty(t, script)
					require.Equal(t, []string{"-n", "-y", "list", "table", "inet", fenceTable}, args)
					if tc.cancelText {
						cancel()
					}
					return []byte(tc.text), tc.textErr
				default:
					t.Fatal("unexpected additional command")
					return nil, nil
				}
			}}
			err := f.Ensure(ctx)
			if tc.wantErr {
				require.ErrorContains(t, err, "installation is unconfirmed")
			} else {
				require.NoError(t, err)
			}
			if tc.textErr != nil {
				require.ErrorIs(t, err, tc.textErr)
			}
			if tc.cancelText {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Equal(t, 3, calls)
		})
	}
}

func TestFenceTextCannotOverrideInvalidJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"null-owner", []byte(strings.Replace(string(fenceJSON()), `"comment":"opensandbox quarantine v1"`, `"comment":null`, 1))},
		{"wrong-owner", []byte(strings.Replace(string(fenceJSON()), fenceOwner, "foreign", 1))},
		{"flags-dormant", []byte(strings.Replace(string(fenceJSON()), `"handle":1`, `"handle":1,"flags":"dormant"`, 1))},
		{"extra-object", []byte(strings.Replace(string(fenceJSON()), `{"metainfo":`, `{"set":{"family":"inet","table":"opensandbox_quarantine","name":"extra"}},{"metainfo":`, 1))},
		{"missing-table", absentFenceJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			f := &Fence{run: func(_ context.Context, script string, args ...string) ([]byte, error) {
				calls++
				if script != "" {
					return nil, nil
				}
				if args[0] == "-j" {
					return tc.data, nil
				}
				return []byte(fenceExpectedTable), nil
			}}
			require.Error(t, f.Ensure(context.Background()))
			require.Equal(t, 2, calls, "invalid/incomplete JSON must fail without a text fallback")
		})
	}
}
