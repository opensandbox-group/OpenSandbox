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
	"math/rand/v2"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
	"github.com/alibaba/opensandbox/internal/safego"
)

// Enforcement-state anchors (issue #2718, OSEP-0029): every enforcement table
// the egress installs natively is written once and, until now, never read back
// while the daemon lives. A table deleted or altered by anything outside the
// egress process left the daemon healthy while traffic silently bypassed
// policy. The AnchorLedger closes that window: at apply time each applier
// records the exact kernel state it just installed (the anchor), and a
// periodic loop re-reads the live ruleset through the same shared verifier the
// quarantine fence uses, restoring drifted tables idempotently and surfacing
// per-table enforcement state (confirmed / drifted / unknown) instead of
// leaving intent and kernel state conflated.

const (
	// DefaultEnforcementInterval is the reconcile cadence. One minute bounds
	// the drift window at the cost of one or two nft invocations per table,
	// measured negligible at pool densities of a few thousand sandboxes.
	DefaultEnforcementInterval = time.Minute
	// enforcementJitter spreads each tick by ±10% so a fleet of egress
	// daemons does not reconcile in lockstep against one host's nftables.
	enforcementJitter = 0.1
	// defaultEnforcementMaxRestoreFailures is how many consecutive failed
	// restorations make a table unrestorable. The threshold is a starting
	// point: the count is exposed as a metric and the escalation hook fires
	// before any containment action is considered.
	defaultEnforcementMaxRestoreFailures = 3
	// enforcementStepTimeout bounds one kernel observation or restore inside
	// the reconcile loop, so a hung nft invocation delays the pass instead of
	// wedging the loop.
	enforcementStepTimeout = 10 * time.Second
)

// EnforcementState is the per-table enforcement status. Gate placement or
// alerting on confirmed; treat unknown as a delay, not a denial — a failed
// observation says nothing about the kernel state.
type EnforcementState string

const (
	// EnforcementConfirmed: the live ruleset exactly matches the anchor.
	EnforcementConfirmed EnforcementState = "confirmed"
	// EnforcementDrifted: a well-formed read differs from the anchor.
	// Restoration has been attempted; repeated failure escalates.
	EnforcementDrifted EnforcementState = "drifted"
	// EnforcementUnknown: the observation itself failed. Never treated as
	// absence or success, and never re-applied on the basis of it.
	EnforcementUnknown EnforcementState = "unknown"
)

// TableAnchor is the in-process expected kernel state for one owned table plus
// the idempotent whole-transaction install that produces it.
type TableAnchor struct {
	// Readback is the exact expected state, verified through the shared
	// verifier (VerifyTable).
	Readback TableReadback
	// Reapply restores the table's current desired state. It must be
	// idempotent and install the complete table as one transaction — the
	// whole-transaction style of the quarantine fence — so a reconcile that
	// races a concurrent apply converges instead of corrupting. Prefer a
	// closure that reads the caller's authoritative desired state over one
	// that replays a captured snapshot: a stale re-apply must never overwrite
	// a legitimate newer policy.
	Reapply func(context.Context) error
}

// TableEnforcement is one table's verification record as surfaced through the
// health and metrics APIs.
type TableEnforcement struct {
	Family              string           `json:"family"`
	Table               string           `json:"table"`
	State               EnforcementState `json:"state"`
	Epoch               uint64           `json:"epoch"`
	ConsecutiveFailures int              `json:"consecutiveFailures"`
	LastVerified        time.Time        `json:"lastVerified"`
	LastError           string           `json:"lastError,omitempty"`
}

// SystemReadbackRunner returns a ReadbackRunner that shells out to the nft
// binary in the caller's network namespace. Diagnostics are preserved on
// failure; a failed command is an error, never an empty ruleset.
func SystemReadbackRunner() ReadbackRunner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "nft", args...)
		var diagnostics bytes.Buffer
		cmd.Stderr = &diagnostics
		output, err := cmd.Output()
		if err != nil {
			return output, fmt.Errorf("nft %s: %w (%s)", strings.Join(args, " "), errors.Join(err, ctx.Err()), strings.TrimSpace(diagnostics.String()))
		}
		return output, nil
	}
}

// SnapshotTable reads the live kernel state of one table and returns the
// TableReadback that describes it exactly: chains and ordered rules from the
// structured JSON ruleset, and the exact normalized textual snapshot. Call it
// immediately after a successful transactional install — the install commits
// atomically, so the snapshot is exactly the state the applier just produced,
// and capturing it doubles as its first verification.
//
// An absent table is an error: there is nothing to anchor. A state the shared
// verifier cannot express (unknown object kinds, unexpected JSON properties)
// fails here, at capture, rather than anchoring an expectation that could
// never verify.
func SnapshotTable(ctx context.Context, run ReadbackRunner, family, table string) (TableReadback, error) {
	out, err := run(ctx, "-j", "list", "ruleset", family)
	if err != nil {
		return TableReadback{}, fmt.Errorf("read table %s %s for anchoring: %w", family, table, err)
	}
	if err := ctx.Err(); err != nil {
		return TableReadback{}, err
	}
	readback, found, err := tableReadbackFromJSON(out, family, table)
	if err != nil {
		return TableReadback{}, fmt.Errorf("anchor table %s %s: %w", family, table, err)
	}
	if !found {
		return TableReadback{}, fmt.Errorf("table %s %s is absent; nothing to anchor", family, table)
	}
	text, err := run(ctx, "-n", "-y", "list", "table", family, table)
	if err != nil {
		return TableReadback{}, fmt.Errorf("read table %s %s text for anchoring: %w", family, table, err)
	}
	if err := ctx.Err(); err != nil {
		return TableReadback{}, err
	}
	readback.Text = string(text)
	// Self-check through the same JSON half the later verifications use, so a
	// shape the verifier rejects surfaces now with its cause attached.
	present, err := InspectTableJSON(out, readback)
	if err != nil {
		return TableReadback{}, fmt.Errorf("captured table %s %s is not verifiable: %w", family, table, err)
	}
	if !present {
		return TableReadback{}, fmt.Errorf("captured table %s %s vanished before it was anchored", family, table)
	}
	// The two reads above are separate nft invocations: a privileged writer
	// that rewrites the table between them would leave readback pairing JSON
	// from one version with text from another — an impossible state no later
	// restore could ever confirm. Verify the assembled snapshot through the
	// full shared verifier (a fresh JSON listing plus fresh text) and reject
	// a capture whose two views no longer agree.
	present, err = VerifyTable(ctx, run, readback)
	if err != nil {
		return TableReadback{}, fmt.Errorf("captured table %s %s changed while it was captured: %w", family, table, err)
	}
	if !present {
		return TableReadback{}, fmt.Errorf("captured table %s %s vanished before it was anchored", family, table)
	}
	return readback, nil
}

// tableReadbackFromJSON extracts the named table's expected state from one
// JSON family ruleset listing. found is false when the table is positively
// absent from a well-formed listing.
func tableReadbackFromJSON(data []byte, family, table string) (TableReadback, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return TableReadback{}, false, fmt.Errorf("decode ruleset: %w", err)
	}
	entries, ok := root["nftables"].([]any)
	if !ok {
		return TableReadback{}, false, errors.New("readback lacks an nftables envelope")
	}
	readback := TableReadback{
		Family: family,
		Table:  table,
		Chains: map[string]ChainReadback{},
		Rules:  map[string][]RuleMatcher{},
	}
	found := false
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || len(entry) != 1 {
			return TableReadback{}, false, errors.New("invalid readback entry")
		}
		for kind, value := range entry {
			object, ok := value.(map[string]any)
			if !ok {
				return TableReadback{}, false, errors.New("invalid readback object")
			}
			objectFamily, _ := object["family"].(string)
			nameKey := "table"
			if kind == "table" {
				nameKey = "name"
			}
			name, _ := object[nameKey].(string)
			if objectFamily != family || name != table {
				continue
			}
			found = true
			switch kind {
			case "table":
				if comment, hasComment := object["comment"].(string); hasComment {
					readback.Owner = comment
				}
			case "chain":
				chainName, _ := object["name"].(string)
				chainType, _ := object["type"].(string)
				hook, _ := object["hook"].(string)
				prio, ok := object["prio"].(json.Number)
				chainPolicy, _ := object["policy"].(string)
				if !ok || chainName == "" || chainType == "" || hook == "" || chainPolicy == "" {
					return TableReadback{}, false, fmt.Errorf("chain %q lacks a complete base-hook description", chainName)
				}
				readback.Chains[chainName] = ChainReadback{Type: chainType, Hook: hook, Prio: prio, Policy: chainPolicy}
			case "rule":
				chainName, _ := object["chain"].(string)
				if chainName == "" {
					return TableReadback{}, false, errors.New("rule lacks a chain")
				}
				readback.Rules[chainName] = append(readback.Rules[chainName], deepEqualExpr(object["expr"]))
			default:
				return TableReadback{}, false, fmt.Errorf("cannot anchor table object %q", kind)
			}
		}
	}
	if !found {
		return TableReadback{}, false, nil
	}
	return readback, true, nil
}

// deepEqualExpr matches a rule whose decoded JSON "expr" equals the captured
// one. Both sides decode with UseNumber, so numeric fields compare exactly.
func deepEqualExpr(want any) RuleMatcher {
	return func(expr any) bool { return reflect.DeepEqual(want, expr) }
}

type anchorKey struct {
	family string
	table  string
}

type anchorState struct {
	anchor    TableAnchor
	epoch     uint64
	state     EnforcementState
	failures  int
	escalated bool
	verified  time.Time
	lastErr   string
}

// AnchorLedger keeps the expected kernel state of opensandbox-owned tables and
// reconciles the live ruleset against it. Anchors are in-process: a daemon
// restart re-installs the tables at startup and the ledger is rebuilt from the
// fresh installs. Only anchored tables are judged and restored; foreign
// ruleset content is never read as evidence about, or mutated into, an
// opensandbox table.
type AnchorLedger struct {
	run ReadbackRunner

	mu      sync.Mutex
	pass    sync.Mutex // serializes reconcile passes
	anchors map[anchorKey]*anchorState

	now            func() time.Time
	maxFailures    int
	onUnrestorable func(TableEnforcement)
	recordDrift    func(family, table string)
}

// NewAnchorLedger returns an empty ledger verifying through run.
func NewAnchorLedger(run ReadbackRunner) *AnchorLedger {
	return &AnchorLedger{
		run:         run,
		anchors:     map[anchorKey]*anchorState{},
		now:         time.Now,
		maxFailures: defaultEnforcementMaxRestoreFailures,
		recordDrift: telemetry.RecordEnforcementDrift,
	}
}

// SetMaxConsecutiveFailures sets how many consecutive failed restorations
// make a table unrestorable. n must be positive.
func (l *AnchorLedger) SetMaxConsecutiveFailures(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxFailures = n
}

// SetUnrestorableHook registers the escalation callback, invoked once per
// failure streak when a table exceeds the consecutive-failure threshold. It
// runs outside the ledger lock and must not call back into the ledger.
func (l *AnchorLedger) SetUnrestorableHook(hook func(TableEnforcement)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onUnrestorable = hook
}

// AnchorTable captures the live state of one owned table and records it as the
// expected state, bumping the epoch so any in-flight verification of the
// previous state is discarded. Call it right after a successful install; the
// capture itself is the install's first verification. A failed capture drops
// any previous anchor for the table instead of retaining it: the reapply
// closure restores the caller's CURRENT desired state, so once a capture fails
// the old snapshot no longer describes what a restore produces, and keeping it
// would make every later reconcile judge correct state as drift and count
// restore failures against it. It must never fail the install that already
// committed; the table is simply left unanchored until the next successful
// capture re-records it.
func (l *AnchorLedger) AnchorTable(ctx context.Context, family, table string, reapply func(context.Context) error) error {
	readback, err := SnapshotTable(ctx, l.run, family, table)
	if err != nil {
		l.DropTable(family, table)
		return err
	}
	l.mu.Lock()
	key := anchorKey{family, table}
	st, ok := l.anchors[key]
	if !ok {
		st = &anchorState{}
		l.anchors[key] = st
	}
	st.epoch++
	st.anchor = TableAnchor{Readback: readback, Reapply: reapply}
	st.state = EnforcementConfirmed
	st.failures = 0
	st.escalated = false
	st.verified = l.now()
	st.lastErr = ""
	l.mu.Unlock()
	return nil
}

// DropTable forgets the anchor after an intentional removal, so the reconcile
// loop never restores a table the daemon deliberately deleted.
func (l *AnchorLedger) DropTable(family, table string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.anchors, anchorKey{family, table})
}

// Enforcement returns the per-table verification records, ordered by family
// and table name for stable health and API output.
func (l *AnchorLedger) Enforcement() []TableEnforcement {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := l.keysLocked()
	out := make([]TableEnforcement, 0, len(keys))
	for _, key := range keys {
		st := l.anchors[key]
		out = append(out, TableEnforcement{
			Family:              key.family,
			Table:               key.table,
			State:               st.state,
			Epoch:               st.epoch,
			ConsecutiveFailures: st.failures,
			LastVerified:        st.verified,
			LastError:           st.lastErr,
		})
	}
	return out
}

// EnforcementStates projects the records for the metrics source: the state
// name plus the consecutive-failure count per table.
func (l *AnchorLedger) EnforcementStates() []telemetry.EnforcementTableState {
	records := l.Enforcement()
	out := make([]telemetry.EnforcementTableState, 0, len(records))
	for _, record := range records {
		out = append(out, telemetry.EnforcementTableState{
			Family:              record.Family,
			Table:               record.Table,
			State:               string(record.State),
			ConsecutiveFailures: int64(record.ConsecutiveFailures),
		})
	}
	return out
}

func (l *AnchorLedger) keysLocked() []anchorKey {
	keys := make([]anchorKey, 0, len(l.anchors))
	for key := range l.anchors {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].family != keys[j].family {
			return keys[i].family < keys[j].family
		}
		return keys[i].table < keys[j].table
	})
	return keys
}

// Run reconciles every anchored table once per interval with jitter, until ctx
// is cancelled. The first pass runs immediately so per-table state is live
// from startup rather than one interval late.
func (l *AnchorLedger) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultEnforcementInterval
	}
	l.ReconcileOnce(ctx)
	timer := time.NewTimer(l.tick(interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			l.ReconcileOnce(ctx)
			timer.Reset(l.tick(interval))
		}
	}
}

// Start runs Run in the background.
func (l *AnchorLedger) Start(ctx context.Context, interval time.Duration) {
	safego.Go(func() { l.Run(ctx, interval) })
}

func (l *AnchorLedger) tick(interval time.Duration) time.Duration {
	skew := 1 + enforcementJitter*(2*rand.Float64()-1)
	return time.Duration(float64(interval) * skew)
}

// ReconcileOnce verifies every anchored table once: match records confirmed;
// a well-formed mismatch or a positively absent table is drift and is
// restored; a failed observation is unknown and is never re-applied on the
// basis of it.
func (l *AnchorLedger) ReconcileOnce(ctx context.Context) {
	l.pass.Lock()
	defer l.pass.Unlock()
	for _, key := range l.keys() {
		if err := ctx.Err(); err != nil {
			return
		}
		l.reconcileTable(ctx, key)
	}
}

func (l *AnchorLedger) keys() []anchorKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.keysLocked()
}

func (l *AnchorLedger) reconcileTable(ctx context.Context, key anchorKey) {
	anchor, epoch, ok := l.current(key)
	if !ok {
		return
	}
	present, err := l.verify(ctx, anchor.Readback)
	if err != nil {
		var mismatch *ReadbackMismatch
		if errors.As(err, &mismatch) {
			// A normal lifecycle change (a subject registering or
			// unregistering re-anchored the table mid-read) legitimately
			// invalidates this observation; that churn is not drift and must
			// not surface as a security alert. The re-check happens here, as
			// close to the signal as possible; restore re-checks again before
			// acting.
			if !l.epochCurrent(key, epoch) {
				return
			}
			log.Warnf("egress enforcement: table %s %s drifted: %v", key.family, key.table, err)
			l.recordDrift(key.family, key.table)
			l.restore(ctx, key, epoch, anchor, err)
			return
		}
		// A failed read is unknown, not absence: never re-applied on the
		// basis of it.
		l.observe(key, epoch, EnforcementUnknown, err)
		return
	}
	if !present {
		// Same epoch guard as the mismatch case: a table deliberately
		// unregistered (anchor dropped) mid-read is churn, not drift.
		if !l.epochCurrent(key, epoch) {
			return
		}
		log.Warnf("egress enforcement: table %s %s is absent; restoring", key.family, key.table)
		l.recordDrift(key.family, key.table)
		l.restore(ctx, key, epoch, anchor, errors.New("table is absent"))
		return
	}
	l.observe(key, epoch, EnforcementConfirmed, nil)
}

func (l *AnchorLedger) current(key anchorKey) (TableAnchor, uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.anchors[key]
	if !ok {
		return TableAnchor{}, 0, false
	}
	return st.anchor, st.epoch, true
}

func (l *AnchorLedger) epochCurrent(key anchorKey, epoch uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.anchors[key]
	return ok && st.epoch == epoch
}

// restore re-applies the expected state and re-verifies. Only a successful
// read-back of the complete expected table resolves an unsuccessful command as
// restored — the quarantine fence's rule — and an epoch change (a concurrent
// policy mutation re-anchored the table) abandons the stale restoration.
func (l *AnchorLedger) restore(ctx context.Context, key anchorKey, epoch uint64, anchor TableAnchor, cause error) {
	// A concurrent policy mutation may have re-anchored this table while the
	// pass was reading it: the drift belongs to superseded expected state and
	// must not be "restored" over the mutation's fresh install.
	if !l.epochCurrent(key, epoch) {
		return
	}
	if anchor.Reapply == nil {
		l.fail(key, epoch, EnforcementDrifted, fmt.Errorf("table %s %s has no restore path: %w", key.family, key.table, cause))
		return
	}
	if err := l.reapply(ctx, anchor.Reapply); err != nil {
		l.fail(key, epoch, EnforcementDrifted, fmt.Errorf("restore of table %s %s failed: %w (%v)", key.family, key.table, err, cause))
		telemetry.RecordEnforcementRestoreFailed(key.family, key.table)
		return
	}
	present, err := l.verify(ctx, anchor.Readback)
	switch {
	case err != nil:
		var mismatch *ReadbackMismatch
		state := EnforcementUnknown
		if errors.As(err, &mismatch) {
			state = EnforcementDrifted
		}
		l.fail(key, epoch, state, fmt.Errorf("restore read-back of table %s %s failed: %w", key.family, key.table, err))
		telemetry.RecordEnforcementRestoreFailed(key.family, key.table)
	case !present:
		l.fail(key, epoch, EnforcementDrifted, fmt.Errorf("restore of table %s %s left the table absent: %w", key.family, key.table, cause))
		telemetry.RecordEnforcementRestoreFailed(key.family, key.table)
	default:
		l.observe(key, epoch, EnforcementConfirmed, nil)
		log.Infof("egress enforcement: table %s %s restored", key.family, key.table)
	}
}

func (l *AnchorLedger) verify(ctx context.Context, want TableReadback) (bool, error) {
	verifyCtx, cancel := context.WithTimeout(ctx, enforcementStepTimeout)
	defer cancel()
	return VerifyTable(verifyCtx, l.run, want)
}

func (l *AnchorLedger) reapply(ctx context.Context, reapply func(context.Context) error) error {
	restoreCtx, cancel := context.WithTimeout(ctx, enforcementStepTimeout)
	defer cancel()
	return reapply(restoreCtx)
}

// observe records a verification outcome unless a newer anchor superseded the
// verified one; a stale read-back must never be mistaken for current state.
func (l *AnchorLedger) observe(key anchorKey, epoch uint64, state EnforcementState, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.anchors[key]
	if !ok || st.epoch != epoch {
		return
	}
	st.state = state
	st.verified = l.now()
	st.lastErr = ""
	if err != nil {
		st.lastErr = err.Error()
	}
	if state == EnforcementConfirmed {
		// A confirmed verification ends the failure streak and re-arms the
		// escalation hook for a future one.
		st.failures = 0
		st.escalated = false
	}
}

// fail records a failed restoration, counting consecutive failures per table
// and escalating once per streak through the unrestorable hook.
func (l *AnchorLedger) fail(key anchorKey, epoch uint64, state EnforcementState, err error) {
	var hook func(TableEnforcement)
	var report TableEnforcement
	l.mu.Lock()
	st, ok := l.anchors[key]
	if ok && st.epoch == epoch {
		st.state = state
		st.failures++
		st.verified = l.now()
		st.lastErr = err.Error()
		if st.failures >= l.maxFailures && !st.escalated {
			st.escalated = true
			hook = l.onUnrestorable
			report = TableEnforcement{
				Family:              key.family,
				Table:               key.table,
				State:               st.state,
				Epoch:               st.epoch,
				ConsecutiveFailures: st.failures,
				LastVerified:        st.verified,
				LastError:           st.lastErr,
			}
		}
	}
	l.mu.Unlock()
	log.Errorf("%v", err)
	if hook != nil {
		hook(report)
	}
}
