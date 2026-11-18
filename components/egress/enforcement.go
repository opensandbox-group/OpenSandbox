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

package main

import (
	"context"
	"os"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/nftables"
	"github.com/alibaba/opensandbox/egress/pkg/telemetry"
)

// Enforcement-state anchors (issue #2718, OSEP-0029). Both profiles record the
// live kernel state of the redirect tables they install natively and reconcile
// it on a loop, surfacing per-table enforcement state instead of conflating
// daemon health with kernel enforcement. The feature is default-off behind
// OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE: when the flag is unset
// newEnforcementLedger returns nil, every anchor call site is a no-op, and the
// process behaves exactly as before.
//
// Coverage is limited to owned tables on the native nft backend
// (OPENSANDBOX_EGRESS_REDIRECT_BACKEND=nft). On the default backend the
// redirects are rules appended to the shared nat OUTPUT chain, where there is
// no owned table to anchor; deletion detection there remains out of scope
// (OSEP-0029 Notes: a rule-scoped iptables reconciliation path is an open
// design decision).

// enforcementAnchorTimeout bounds the capture of one table's live state during
// a control-plane mutation, so anchor bookkeeping can never stall it.
const enforcementAnchorTimeout = 5 * time.Second

// newEnforcementLedger builds the anchor ledger when the reconcile feature is
// enabled. It must be called before the installs: the appliers record anchors
// as they install. A nil return (feature off) disables anchoring everywhere.
func newEnforcementLedger() *nftables.AnchorLedger {
	if !constants.IsTruthy(os.Getenv(constants.EnvEnforcementReconcile)) {
		return nil
	}
	ledger := nftables.NewAnchorLedger(nftables.SystemReadbackRunner())
	maxFailures := constants.EnvIntRange(constants.EnvEnforcementMaxRestoreFailures,
		constants.DefaultEnforcementMaxRestoreFailures, 1, 100)
	ledger.SetMaxConsecutiveFailures(maxFailures)
	// Escalation for the first increment is observable, not behavioral: a loud
	// error per streak, the state and consecutive-failure gauges, and the
	// per-table state on the health surfaces. Containment escalation needs N
	// tuned with production data (OSEP-0029 Risks) and stays out until then.
	ledger.SetUnrestorableHook(func(record nftables.TableEnforcement) {
		log.Errorf("egress enforcement: table %s %s is unrestorable after %d consecutive failed restorations: %s",
			record.Family, record.Table, record.ConsecutiveFailures, record.LastError)
	})
	telemetry.SetEnforcementStateSource(ledger.EnforcementStates)
	log.Infof("egress enforcement-state anchors enabled (reconcile ~%s, escalate after %d consecutive failed restorations)",
		enforcementReconcileInterval(), maxFailures)
	return ledger
}

// startEnforcementReconcile starts the periodic loop. Nil (feature off) is a
// no-op. The loop's first pass runs immediately, so per-table state is live
// from startup rather than one interval late.
func startEnforcementReconcile(ctx context.Context, ledger *nftables.AnchorLedger) {
	if ledger == nil {
		return
	}
	interval := enforcementReconcileInterval()
	ledger.Start(ctx, interval)
	log.Infof("egress enforcement reconcile started (interval ~%s)", interval)
}

func enforcementReconcileInterval() time.Duration {
	seconds := constants.EnvIntRange(constants.EnvEnforcementReconcileIntervalSec,
		constants.DefaultEnforcementReconcileIntervalSec, 1, 3600)
	return time.Duration(seconds) * time.Second
}
