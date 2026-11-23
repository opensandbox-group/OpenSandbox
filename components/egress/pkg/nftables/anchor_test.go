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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeKernel is an in-memory ruleset: it answers the verifier's two listing
// commands from mutable fields so tests can script drift, absence and read
// failures between reconcile passes. Tests drive it sequentially; the one
// concurrent test (Run) only touches atomic fields.
type fakeKernel struct {
	ruleset   []byte // answer to `-j list ruleset <family>`
	table     []byte // answer to `-n -y list table <family> <table>`
	jsonErr   error
	textErr   error
	reapplies int
	onJSON    func() // invoked before each JSON listing; must not re-enter run
}

func (k *fakeKernel) run(_ context.Context, args ...string) ([]byte, error) {
	if args[0] == "-j" {
		if k.onJSON != nil {
			k.onJSON()
		}
		if k.jsonErr != nil {
			return nil, k.jsonErr
		}
		return k.ruleset, nil
	}
	if k.textErr != nil {
		return nil, k.textErr
	}
	return k.table, nil
}

// dropSecondRule removes the second rule of the fixture's only chain from both
// listings, simulating an external deletion of exactly one rule.
func dropSecondRule(t *testing.T) (ruleset, table []byte) {
	t.Helper()
	var doc struct {
		Nftables []map[string]any `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal(multiRuleJSON(), &doc))
	doc.Nftables = append(doc.Nftables[:4], doc.Nftables[5:]...)
	ruleset, err := json.Marshal(doc)
	require.NoError(t, err)
	table = []byte(strings.Replace(string(multiRuleReadback().Text),
		"\t\tredirect to :15353\n", "", 1))
	return ruleset, table
}

func TestSnapshotTableCapturesLiveState(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	readback, err := SnapshotTable(context.Background(), kernel.run, "ip", "opensandbox_multi")
	require.NoError(t, err)
	require.Equal(t, "ip", readback.Family)
	require.Equal(t, "opensandbox_multi", readback.Table)
	require.Len(t, readback.Chains, 1)
	require.Len(t, readback.Rules["output"], 2)

	// The capture must verify against the state it was taken from, and keep
	// verifying while the kernel holds that state.
	present, err := VerifyTable(context.Background(), kernel.run, readback)
	require.NoError(t, err)
	require.True(t, present)

	// One rule deleted externally must break it.
	kernel.ruleset, kernel.table = dropSecondRule(t)
	present, err = VerifyTable(context.Background(), kernel.run, readback)
	require.Error(t, err, "a deleted rule must not verify against the captured anchor")
	var mismatch *ReadbackMismatch
	require.True(t, errors.As(err, &mismatch))
	require.False(t, present)
}

func TestSnapshotTableAbsentTableIsAnError(t *testing.T) {
	kernel := &fakeKernel{
		ruleset: []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":"ip","name":"foreign"}}]}`),
		table:   []byte(""),
	}
	_, err := SnapshotTable(context.Background(), kernel.run, "ip", "opensandbox_multi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "absent")
}

func TestSnapshotTableRejectsUnverifiableShape(t *testing.T) {
	// A set object inside the owned table cannot be expressed by the shared
	// verifier; capturing it must fail loudly instead of anchoring an
	// expectation that could never verify.
	polluted := []byte(strings.Replace(string(multiRuleJSON()),
		`{"rule":{"family":"ip","table":"opensandbox_multi","chain":"output","handle":4,"expr":[{"redirect":{"port":15353}}]}}`,
		`{"rule":{"family":"ip","table":"opensandbox_multi","chain":"output","handle":4,"expr":[{"redirect":{"port":15353}}]}},{"set":{"family":"ip","table":"opensandbox_multi","name":"foreign_set"}}`, 1))
	kernel := &fakeKernel{ruleset: polluted, table: []byte(multiRuleReadback().Text)}
	_, err := SnapshotTable(context.Background(), kernel.run, "ip", "opensandbox_multi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "cannot anchor table object")

	// A base chain property the verifier rejects (a device binding) parses
	// into the snapshot but fails the capture self-check.
	deviceBound := []byte(strings.Replace(string(multiRuleJSON()),
		`"type":"nat","hook":"output","prio":-100,"policy":"accept"`,
		`"type":"nat","hook":"output","prio":-100,"policy":"accept","dev":"eth0"`, 1))
	kernel = &fakeKernel{ruleset: deviceBound, table: []byte(multiRuleReadback().Text)}
	_, err = SnapshotTable(context.Background(), kernel.run, "ip", "opensandbox_multi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not verifiable")
}

func TestSnapshotTableRejectsTableChangedBetweenReads(t *testing.T) {
	// A privileged writer rewrites the table between the capture's JSON
	// listing and its textual read: the assembled snapshot would pair JSON
	// from the old version with text from the new one — an impossible state
	// no later restore could ever confirm. The capture must reject it
	// instead of anchoring it.
	jsonA := multiRuleJSON()
	jsonB, textB := dropSecondRule(t)
	calls := 0
	run := func(_ context.Context, _ ...string) ([]byte, error) {
		calls++
		switch calls {
		case 1:
			return jsonA, nil // capture's JSON listing: old version
		case 2:
			return textB, nil // capture's text read: the writer's new version
		case 3:
			return jsonB, nil // fresh verification JSON: new version
		default:
			return textB, nil // fresh verification text: new version
		}
	}
	_, err := SnapshotTable(context.Background(), run, "ip", "opensandbox_multi")
	require.Error(t, err)
	require.Contains(t, err.Error(), "changed while it was captured")
}

func TestSnapshotTableDecodeFailureIsAnError(t *testing.T) {
	kernel := &fakeKernel{ruleset: []byte(`{"nftables":`), table: []byte("")}
	_, err := SnapshotTable(context.Background(), kernel.run, "ip", "opensandbox_multi")
	require.Error(t, err)
}

func TestAnchorLedgerConfirmAndRestoreOneDriftedRule(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	driftSignals := 0
	ledger.recordDrift = func(_, _ string) { driftSignals++ }
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		kernel.ruleset = multiRuleJSON()
		kernel.table = []byte(multiRuleReadback().Text)
		return nil
	}))

	records := ledger.Enforcement()
	require.Len(t, records, 1)
	require.Equal(t, EnforcementConfirmed, records[0].State)
	require.Equal(t, uint64(1), records[0].Epoch)
	require.Zero(t, records[0].ConsecutiveFailures)

	// One external rule deletion: detected, restored, and re-confirmed.
	kernel.ruleset, kernel.table = dropSecondRule(t)
	ledger.ReconcileOnce(context.Background())
	require.Equal(t, 1, kernel.reapplies)
	records = ledger.Enforcement()
	require.Equal(t, EnforcementConfirmed, records[0].State)
	require.Zero(t, records[0].ConsecutiveFailures)
	require.Empty(t, records[0].LastError)

	// An already-correct table is a no-op restore.
	ledger.ReconcileOnce(context.Background())
	require.Equal(t, 1, kernel.reapplies, "an idempotent reconcile must not re-apply a matching table")
	require.Equal(t, 1, driftSignals, "real drift is signalled once; a confirmed pass signals nothing")
}

func TestAnchorLedgerAbsentTableIsDriftAndIsRestored(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		kernel.ruleset = multiRuleJSON()
		kernel.table = []byte(multiRuleReadback().Text)
		return nil
	}))

	// `nft flush ruleset` class: the table disappears entirely.
	kernel.ruleset = []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}}]}`)
	kernel.table = []byte("")
	ledger.ReconcileOnce(context.Background())
	require.Equal(t, 1, kernel.reapplies)
	require.Equal(t, EnforcementConfirmed, ledger.Enforcement()[0].State)
}

func TestAnchorLedgerUnknownObservationNeverReapplies(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		return nil
	}))

	// A failed list is unknown: it asserts neither absence nor drift, and no
	// restore may run on the basis of it.
	kernel.jsonErr = errors.New("nft list failed")
	ledger.ReconcileOnce(context.Background())
	records := ledger.Enforcement()
	require.Equal(t, EnforcementUnknown, records[0].State)
	require.Zero(t, kernel.reapplies)
	require.Zero(t, records[0].ConsecutiveFailures)
	require.NotEmpty(t, records[0].LastError)

	// Recovery: the next successful read confirms again.
	kernel.jsonErr = nil
	ledger.ReconcileOnce(context.Background())
	require.Equal(t, EnforcementConfirmed, ledger.Enforcement()[0].State)
	require.Zero(t, kernel.reapplies)
}

func TestAnchorLedgerRestoreFailuresEscalateOncePerStreak(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	// A restore that never converges: the kernel stays drifted.
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		return nil
	}))
	kernel.ruleset, kernel.table = dropSecondRule(t)

	var reports []TableEnforcement
	var mu sync.Mutex
	reportsSoFar := func() []TableEnforcement {
		mu.Lock()
		defer mu.Unlock()
		return append([]TableEnforcement(nil), reports...)
	}
	ledger.SetMaxConsecutiveFailures(2)
	ledger.SetUnrestorableHook(func(record TableEnforcement) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, record)
	})

	for range 3 {
		ledger.ReconcileOnce(context.Background())
	}
	got := reportsSoFar()
	require.Len(t, got, 1, "the escalation hook fires once per failure streak")
	require.Equal(t, 2, got[0].ConsecutiveFailures)
	records := ledger.Enforcement()
	require.Equal(t, EnforcementDrifted, records[0].State)
	require.Equal(t, 3, records[0].ConsecutiveFailures)
	require.NotEmpty(t, records[0].LastError)

	// A confirmed verification resets the streak and re-arms the hook.
	kernel.ruleset = multiRuleJSON()
	kernel.table = []byte(multiRuleReadback().Text)
	ledger.ReconcileOnce(context.Background())
	require.Equal(t, EnforcementConfirmed, ledger.Enforcement()[0].State)
	require.Zero(t, ledger.Enforcement()[0].ConsecutiveFailures)
	kernel.ruleset, kernel.table = dropSecondRule(t)
	for range 2 {
		ledger.ReconcileOnce(context.Background())
	}
	require.Len(t, reportsSoFar(), 2, "a new streak escalates again")
}

func TestAnchorLedgerAnchorWithoutRestorePathCountsFailures(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", nil))
	kernel.ruleset, kernel.table = dropSecondRule(t)
	ledger.ReconcileOnce(context.Background())
	records := ledger.Enforcement()
	require.Equal(t, EnforcementDrifted, records[0].State)
	require.Equal(t, 1, records[0].ConsecutiveFailures)
	require.Contains(t, records[0].LastError, "no restore path")
}

func TestAnchorLedgerStaleEpochIsDiscarded(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	driftSignals := 0
	ledger.recordDrift = func(_, _ string) { driftSignals++ }
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		return nil
	}))

	// A policy mutation re-anchors the table in the middle of the reconcile
	// pass: the in-flight verification belongs to the old epoch, so the old
	// restore must not run over the mutation's fresh install, and the stale
	// verification must not overwrite the new anchor's record.
	kernel.onJSON = func() {
		kernel.onJSON = nil
		require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
			kernel.reapplies++
			return nil
		}))
	}
	ledger.ReconcileOnce(context.Background())
	kernel.onJSON = nil
	records := ledger.Enforcement()
	require.Equal(t, uint64(2), records[0].Epoch)
	require.Equal(t, EnforcementConfirmed, records[0].State)
	require.Zero(t, kernel.reapplies, "a stale restore must never run against a newer anchor")

	// The same race with real drift: the pass reads a mismatching table, but
	// the table was re-anchored meanwhile — the stale drift is not restored,
	// and the new anchor's confirmed record stands.
	kernel.ruleset, kernel.table = dropSecondRule(t)
	kernel.onJSON = func() {
		kernel.onJSON = nil
		require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
			kernel.reapplies++
			return nil
		}))
	}
	ledger.ReconcileOnce(context.Background())
	kernel.onJSON = nil
	records = ledger.Enforcement()
	require.Equal(t, uint64(3), records[0].Epoch)
	require.Equal(t, EnforcementConfirmed, records[0].State)
	require.Zero(t, kernel.reapplies, "drift against a superseded anchor must not be restored")
	require.Zero(t, driftSignals, "ordinary lifecycle churn must not surface as drift")
}

func TestAnchorLedgerDropTableStopsRestoring(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		return nil
	}))
	// Intentional removal: the anchor goes with the table, so no reconcile
	// restores it.
	ledger.DropTable("ip", "opensandbox_multi")
	kernel.ruleset = []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}}]}`)
	kernel.table = []byte("")
	ledger.ReconcileOnce(context.Background())
	require.Zero(t, kernel.reapplies)
	require.Empty(t, ledger.Enforcement())
}

func TestAnchorLedgerFailedCaptureInvalidatesPreviousAnchor(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		return nil
	}))

	// A mutation installs the caller's newest desired state (the kernel now
	// holds a state the old snapshot does not describe), but its re-anchor
	// capture fails. The superseded anchor must be dropped: retaining it would
	// make every following reconcile read the correctly installed table,
	// judge it drifted against the stale snapshot, and burn restore failures
	// (and eventually escalate) until the next capture succeeds.
	kernel.ruleset, kernel.table = dropSecondRule(t)
	kernel.jsonErr = errors.New("nft list failed")
	require.Error(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		return nil
	}))
	kernel.jsonErr = nil

	require.Empty(t, ledger.Enforcement(), "a failed capture must not leave the previous anchor in place")
	ledger.ReconcileOnce(context.Background())
	require.Zero(t, kernel.reapplies, "the loop must never restore against a stale anchor")

	// The next successful capture re-arms verification from live state.
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", func(_ context.Context) error {
		kernel.reapplies++
		return nil
	}))
	records := ledger.Enforcement()
	require.Len(t, records, 1)
	require.Equal(t, EnforcementConfirmed, records[0].State)
}

func minimalTableJSON(family, table string) []byte {
	return []byte(fmt.Sprintf(`{"nftables":[{"metainfo":{"json_schema_version":1}},{"table":{"family":%q,"name":%q,"handle":1}},{"chain":{"family":%q,"table":%q,"name":"output","handle":2,"type":"nat","hook":"output","prio":-100,"policy":"accept"}}]}`, family, table, family, table))
}

func TestAnchorLedgerReportsTablesSorted(t *testing.T) {
	// One shared runner serving a minimal table per family listing.
	rulesets := map[string][]byte{
		"ip":   minimalTableJSON("ip", "opensandbox_multi"),
		"ip6":  minimalTableJSON("ip6", "opensandbox_multi"),
		"inet": minimalTableJSON("inet", "opensandbox_other"),
	}
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "-j" {
			return rulesets[args[len(args)-1]], nil
		}
		return []byte("table listing text"), nil
	}
	ledger := NewAnchorLedger(run)
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip6", "opensandbox_multi", nil))
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", nil))
	require.NoError(t, ledger.AnchorTable(context.Background(), "inet", "opensandbox_other", nil))
	records := ledger.Enforcement()
	require.Len(t, records, 3)
	require.Equal(t, "inet", records[0].Family)
	require.Equal(t, "opensandbox_other", records[0].Table)
	require.Equal(t, "ip", records[1].Family)
	require.Equal(t, "ip6", records[2].Family)
}

func TestAnchorLedgerRunStopsOnCancel(t *testing.T) {
	var reconciles atomic.Int64
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "-j" {
			reconciles.Add(1)
		}
		return kernel.run(context.Background(), args...)
	}
	ledger := NewAnchorLedger(run)
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ledger.Run(ctx, 5*time.Millisecond)
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
	require.Positive(t, reconciles.Load(), "the loop must reconcile at least once")
}

func TestAnchorLedgerSetMaxConsecutiveFailuresRejectsNonPositive(t *testing.T) {
	kernel := &fakeKernel{ruleset: multiRuleJSON(), table: []byte(multiRuleReadback().Text)}
	ledger := NewAnchorLedger(kernel.run)
	ledger.SetMaxConsecutiveFailures(0)
	var called atomic.Bool
	ledger.SetUnrestorableHook(func(TableEnforcement) { called.Store(true) })
	require.NoError(t, ledger.AnchorTable(context.Background(), "ip", "opensandbox_multi", nil))
	kernel.ruleset, kernel.table = dropSecondRule(t)
	// maxFailures clamped to 1: the first failed restoration escalates.
	ledger.ReconcileOnce(context.Background())
	require.True(t, called.Load())
}
