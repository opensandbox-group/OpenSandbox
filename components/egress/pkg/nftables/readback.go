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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ReadbackRunner runs one nft list command and returns its stdout. A failed
// command must be reported as an error, never as an empty ruleset: callers
// treat a failed read as unknown, never as proof that a table is absent.
type ReadbackRunner func(ctx context.Context, args ...string) ([]byte, error)

// RuleMatcher reports whether a rule's decoded JSON "expr" value matches the
// expected rule for its chain.
type RuleMatcher func(expr any) bool

// ChainReadback is the exact base chain expected in a table: its type, hook,
// priority and policy must all match.
type ChainReadback struct {
	Type   string
	Hook   string
	Prio   json.Number
	Policy string
}

// TableReadback is the exact kernel state expected for one nftables table.
// It is the shared verification primitive for every opensandbox-owned table:
// the structured JSON ruleset and an exact normalized textual snapshot are
// both required, so a successful readback is positive confirmation and any
// failure stays unknown. Only the named table is judged; foreign tables and
// other families are ignored, never modified.
type TableReadback struct {
	// Family is the nftables family the table lives in, e.g. "inet".
	Family string
	// Table is the table name, e.g. "opensandbox_quarantine".
	Table string
	// Owner is the expected table comment. Empty skips the JSON comment check
	// (older nft omits table comments in JSON); the textual snapshot still
	// enforces ownership.
	Owner string
	// Chains is the exact set of base chains the table must contain.
	Chains map[string]ChainReadback
	// Rules maps each chain that must hold a rule to the matcher for that
	// rule's "expr" value. A rule in a chain with no entry here is rejected.
	Rules map[string]RuleMatcher
	// Text is the exact expected textual snapshot of the whole table, as
	// printed by `nft -n -y list table`. Whitespace differences that do not
	// change a line's content are ignored; anything else is a mismatch.
	Text string
}

// UnconditionalDrop reports whether expr is exactly one unconditional drop
// verdict: [{"drop":null}].
func UnconditionalDrop(expr any) bool {
	expressions, ok := expr.([]any)
	if !ok || len(expressions) != 1 {
		return false
	}
	expression, ok := expressions[0].(map[string]any)
	if !ok || len(expression) != 1 {
		return false
	}
	drop, ok := expression["drop"]
	return ok && drop == nil
}

// VerifyTable reads the live kernel state for one table and compares it
// against the expected snapshot. It returns (true, nil) only when both the
// structured JSON ruleset and the exact normalized textual snapshot match.
// (false, nil) means the table is positively absent: the JSON readback
// succeeded and contained no object for the table. Any other outcome — a
// failed list command, malformed JSON, a mismatching object, or a textual
// mismatch — is an error: a failed read must never be parsed as proof of
// absence, and a mismatch must never be reported as a clean absence.
func VerifyTable(ctx context.Context, run ReadbackRunner, want TableReadback) (bool, error) {
	// Listing the whole family makes absence a positive, successful kernel
	// response. A failed "list ruleset" must never be parsed as proof of absence.
	out, err := run(ctx, "-j", "list", "ruleset", want.Family)
	if err != nil {
		return false, fmt.Errorf("read table %q kernel state: %w", want.Table, err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	present, err := InspectTableJSON(out, want)
	if err != nil || !present {
		return false, err
	}
	// Older nft omits table comments in JSON and has a buggy single-flag JSON
	// printer. Require an independent, complete fixed textual snapshot as well.
	text, err := run(ctx, "-n", "-y", "list", "table", want.Family, want.Table)
	if err != nil {
		return false, fmt.Errorf("read table %q text state: %w", want.Table, err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !exactTableText(text, want.Text) {
		return false, fmt.Errorf("table %q text state does not exactly match the required table", want.Table)
	}
	return true, nil
}

func exactTableText(data []byte, expected string) bool {
	normalize := func(text string) string {
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	return normalize(string(data)) == normalize(expected)
}

// InspectTableJSON checks the exact JSON table, its base chains and their
// rules. Old nft omits table comments, so this check alone is not
// confirmation: VerifyTable also requires the complete textual table,
// including its exact owner. Extra flags, expressions, chains, objects or
// unknown properties are rejected.
func InspectTableJSON(data []byte, want TableReadback) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return false, fmt.Errorf("decode table %q readback: %w", want.Table, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return false, fmt.Errorf("table %q readback contains trailing data", want.Table)
	}
	entries, ok := root["nftables"].([]any)
	if !ok || len(root) != 1 {
		return false, fmt.Errorf("table %q readback lacks an exact nftables envelope", want.Table)
	}
	tables := 0
	chains, rules := map[string]int{}, map[string]int{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || len(entry) != 1 {
			return false, fmt.Errorf("invalid table %q readback entry", want.Table)
		}
		for kind, value := range entry {
			object, ok := value.(map[string]any)
			if !ok {
				return false, fmt.Errorf("invalid table %q readback object", want.Table)
			}
			if kind == "metainfo" {
				continue
			}
			family, hasFamily := object["family"].(string)
			if !hasFamily {
				return false, errors.New("readback object has no family")
			}
			if family != want.Family {
				continue
			}
			nameKey := "table"
			if kind == "table" {
				nameKey = "name"
			}
			name, hasName := object[nameKey].(string)
			if !hasName {
				return false, errors.New("readback object has no table identity")
			}
			if name != want.Table {
				continue
			}
			switch kind {
			case "table":
				tables++
				comment, hasComment := object["comment"]
				if !onlyKeys(object, "family", "name", "handle", "comment", "flags") || (want.Owner != "" && hasComment && comment != want.Owner) || !emptyFlags(object) {
					return false, fmt.Errorf("table %q ownership or flags do not match", want.Table)
				}
			case "chain":
				chain, _ := object["name"].(string)
				expected, known := want.Chains[chain]
				if !known || !onlyKeys(object, "family", "table", "name", "handle", "type", "hook", "prio", "policy") || object["type"] != expected.Type || object["hook"] != expected.Hook || object["prio"] != expected.Prio || object["policy"] != expected.Policy {
					return false, fmt.Errorf("table %q chain does not match the required base hook", want.Table)
				}
				chains[chain]++
			case "rule":
				chain, _ := object["chain"].(string)
				matcher, known := want.Rules[chain]
				if !known || matcher == nil || !onlyKeys(object, "family", "table", "chain", "handle", "expr") || !matcher(object["expr"]) {
					return false, fmt.Errorf("table %q rule does not match the required rule", want.Table)
				}
				rules[chain]++
			default:
				return false, fmt.Errorf("unexpected table %q object %q", want.Table, kind)
			}
		}
	}
	if tables == 0 && len(chains) == 0 && len(rules) == 0 {
		return false, nil
	}
	if tables != 1 {
		return false, fmt.Errorf("table %q readback has an invalid table count", want.Table)
	}
	for hook := range want.Chains {
		if chains[hook] != 1 {
			return false, fmt.Errorf("table %q readback lacks the exact %s chain", want.Table, hook)
		}
	}
	for chain := range want.Rules {
		if rules[chain] != 1 {
			return false, fmt.Errorf("table %q readback lacks the exact %s rule", want.Table, chain)
		}
	}
	return true, nil
}

func onlyKeys(object map[string]any, keys ...string) bool {
	for key := range object {
		found := false
		for _, allowed := range keys {
			if key == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func emptyFlags(object map[string]any) bool {
	flags, exists := object["flags"]
	if !exists {
		return true
	}
	values, ok := flags.([]any)
	return ok && len(values) == 0
}
