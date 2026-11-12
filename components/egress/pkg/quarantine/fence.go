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
	"os/exec"
	"strings"
	"sync"

	"github.com/alibaba/opensandbox/egress/pkg/nftables"
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
	return nftables.VerifyTable(ctx, func(ctx context.Context, args ...string) ([]byte, error) {
		return f.run(ctx, "", args...)
	}, fenceTableReadback)
}

// fenceTableReadback is the exact kernel state Fence.Ensure verifies: the
// quarantine table with its three drop-all base chains, checked both as a
// structured JSON ruleset and as an exact normalized textual snapshot.
var fenceTableReadback = nftables.TableReadback{
	Family: "inet",
	Table:  fenceTable,
	Owner:  fenceOwner,
	Chains: map[string]nftables.ChainReadback{
		"input":   {Type: "filter", Hook: "input", Prio: json.Number("-450"), Policy: "drop"},
		"output":  {Type: "filter", Hook: "output", Prio: json.Number("-450"), Policy: "drop"},
		"forward": {Type: "filter", Hook: "forward", Prio: json.Number("-450"), Policy: "drop"},
	},
	Rules: map[string]nftables.RuleMatcher{
		"input":   nftables.UnconditionalDrop,
		"output":  nftables.UnconditionalDrop,
		"forward": nftables.UnconditionalDrop,
	},
	Text: fenceExpectedTable,
}
