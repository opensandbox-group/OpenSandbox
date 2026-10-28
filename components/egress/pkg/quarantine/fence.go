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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

const (
	fenceTable = "opensandbox_quarantine"
	fenceOwner = "opensandbox quarantine v1"
)

// Fence is an independent, namespace-wide IPv4/IPv6 packet quarantine. Its
// table is deliberately separate from the normal policy and redirect tables.
// It has no UID, mark, loopback, established-connection or proxy exceptions.
//
// This boundary assumes the workload cannot modify the namespace's network
// administration or use raw/L2 or offloaded paths to bypass these inet hooks.
// The owner must serialize lifecycle state with Ensure. A successful
// operation reports a verified kernel snapshot, not protection from another
// privileged writer modifying the table afterward.
type Fence struct {
	mu  sync.Mutex
	run fenceRunner
}

type fenceRunner func(context.Context, string, ...string) ([]byte, error)

// NewFence constructs the production fence in the caller's network namespace.
func NewFence() *Fence { return &Fence{run: runFenceNft} }

// Ensure atomically installs the complete fence and verifies its exact kernel
// representation. An unsuccessful command may have committed; only successful
// readback of the complete fence can resolve that uncertainty as success.
func (f *Fence) Ensure(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	_, applyErr := f.run(ctx, fenceInstallScript, "-f", "-")
	present, readErr := f.read(ctx)
	if readErr == nil && present {
		return nil
	}
	if readErr == nil {
		readErr = errors.New("quarantine table is absent")
	}
	return fmt.Errorf("packet quarantine installation is unconfirmed: %w", errors.Join(applyErr, readErr))
}

// "add" is idempotent for an existing table. The whole input is one nft
// transaction, including add/delete/recreate, so missing and existing tables
// need neither a racy preliminary lookup nor stderr-based missing-table guesses.
// Priority -450 is before ordinary conntrack/NAT/filter processing at the same
// hook. Input and forward necessarily follow their earlier prerouting hook.
const fenceInstallScript = `add table inet opensandbox_quarantine
delete table inet opensandbox_quarantine
` + fenceExpectedTable

const fenceExpectedTable = `table inet opensandbox_quarantine {
    comment "opensandbox quarantine v1"
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

func runFenceNft(ctx context.Context, script string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "nft", args...)
	if script != "" {
		cmd.Stdin = strings.NewReader(script)
	}
	var diagnostics bytes.Buffer
	cmd.Stderr = &diagnostics
	output, err := cmd.Output()
	if err != nil {
		return output, fmt.Errorf("nft %s: %w (%s)", strings.Join(args, " "), errors.Join(err, ctx.Err()), strings.TrimSpace(diagnostics.String()))
	}
	return output, nil
}

func (f *Fence) read(ctx context.Context) (bool, error) {
	// Listing all inet tables makes absence a positive, successful kernel
	// response. A failed "list table" must never be parsed as proof of absence.
	out, err := f.run(ctx, "", "-j", "list", "ruleset", "inet")
	if err != nil {
		return false, fmt.Errorf("read quarantine kernel state: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	present, err := inspectFence(out)
	if err != nil || !present {
		return false, err
	}
	// Older nft omits table comments in JSON and has a buggy single-flag JSON
	// printer. Require an independent, complete fixed textual snapshot as well.
	text, err := f.run(ctx, "", "-n", "-y", "list", "table", "inet", fenceTable)
	if err != nil {
		return false, fmt.Errorf("read quarantine text state: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !exactFenceText(text) {
		return false, errors.New("quarantine text state does not exactly match the required fence")
	}
	return true, nil
}

func exactFenceText(data []byte) bool {
	normalize := func(text string) string {
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	return normalize(string(data)) == normalize(fenceExpectedTable)
}

// inspectFence checks the exact JSON table, three base chains and three drop
// rules. Old nft omits table comments, so this check alone is not confirmation:
// read also requires the complete textual table, including its exact owner.
// Extra flags, expressions, chains, objects or unknown properties are rejected.
func inspectFence(data []byte) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return false, fmt.Errorf("decode quarantine readback: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return false, errors.New("quarantine readback contains trailing data")
	}
	entries, ok := root["nftables"].([]any)
	if !ok || len(root) != 1 {
		return false, errors.New("quarantine readback lacks an exact nftables envelope")
	}
	tables := 0
	chains, rules := map[string]int{}, map[string]int{}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || len(entry) != 1 {
			return false, errors.New("invalid quarantine readback entry")
		}
		for kind, value := range entry {
			object, ok := value.(map[string]any)
			if !ok {
				return false, errors.New("invalid quarantine readback object")
			}
			if kind == "metainfo" {
				continue
			}
			family, hasFamily := object["family"].(string)
			if !hasFamily {
				return false, errors.New("readback object has no family")
			}
			if family != "inet" {
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
			if name != fenceTable {
				continue
			}
			switch kind {
			case "table":
				tables++
				comment, hasComment := object["comment"]
				if !onlyKeys(object, "family", "name", "handle", "comment", "flags") || (hasComment && comment != fenceOwner) || !emptyFlags(object) {
					return false, errors.New("quarantine table ownership or flags do not match")
				}
			case "chain":
				chain, _ := object["name"].(string)
				if !fenceHook(chain) || !onlyKeys(object, "family", "table", "name", "handle", "type", "hook", "prio", "policy") || object["type"] != "filter" || object["hook"] != chain || object["prio"] != json.Number("-450") || object["policy"] != "drop" {
					return false, errors.New("quarantine chain does not match the required base hook")
				}
				chains[chain]++
			case "rule":
				chain, _ := object["chain"].(string)
				if !fenceHook(chain) || !onlyKeys(object, "family", "table", "chain", "handle", "expr") || !unconditionalDrop(object["expr"]) {
					return false, errors.New("quarantine rule is not the required unconditional drop")
				}
				rules[chain]++
			default:
				return false, fmt.Errorf("unexpected quarantine object %q", kind)
			}
		}
	}
	if tables == 0 && len(chains) == 0 && len(rules) == 0 {
		return false, nil
	}
	if tables != 1 {
		return false, errors.New("quarantine readback has an invalid table count")
	}
	for _, hook := range []string{"input", "output", "forward"} {
		if chains[hook] != 1 || rules[hook] != 1 {
			return false, fmt.Errorf("quarantine readback lacks the exact %s chain and rule", hook)
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

func fenceHook(hook string) bool {
	return hook == "input" || hook == "output" || hook == "forward"
}

func unconditionalDrop(value any) bool {
	expressions, ok := value.([]any)
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
