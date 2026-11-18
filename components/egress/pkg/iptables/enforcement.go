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

package iptables

import (
	"context"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/log"
)

// EnforcementAnchors records the live kernel state of the redirect tables this
// package installs natively, so the egress daemon can detect and repair drift
// while it runs (issue #2718, OSEP-0029). The anchor ledger implements it; a
// nil value (the feature is default-off) disables recording entirely.
type EnforcementAnchors interface {
	// AnchorTable captures the live state of the named table and records it as
	// the expected state. reapply must restore the table's current desired
	// state idempotently, as one whole transaction.
	AnchorTable(ctx context.Context, family, table string, reapply func(context.Context) error) error
	// DropTable forgets the anchor after an intentional removal, so a
	// reconcile loop never restores a table the daemon deliberately deleted.
	DropTable(family, table string)
}

// enforcementAnchorTimeout bounds the capture of one table's live state so an
// anchor recording can never stall the install path that already committed.
const enforcementAnchorTimeout = 5 * time.Second

// anchorRedirectTable records one anchor. The install already succeeded; a
// missing anchor only degrades drift detection until the next mutation
// re-anchors, so it is logged and never fails the caller.
func anchorRedirectTable(ctx context.Context, anchors EnforcementAnchors, family, table string, reapply func(context.Context) error) {
	if anchors == nil {
		return
	}
	anchorCtx, cancel := context.WithTimeout(ctx, enforcementAnchorTimeout)
	defer cancel()
	if err := anchors.AnchorTable(anchorCtx, family, table, reapply); err != nil {
		log.Warnf("egress enforcement: anchor for table %s %s not recorded: %v", family, table, err)
	}
}

func dropRedirectAnchor(anchors EnforcementAnchors, family, table string) {
	if anchors != nil {
		anchors.DropTable(family, table)
	}
}
