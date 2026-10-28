// Copyright 2026 The OpenSandbox Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/log"
	"github.com/alibaba/opensandbox/egress/pkg/quarantine"
)

type packetQuarantineState uint32

const (
	quarantineInactive packetQuarantineState = iota
	quarantineUnknown
	quarantineConfirmed
)

func (p packetQuarantineState) String() string {
	switch p {
	case quarantineConfirmed:
		return "QuarantineConfirmed"
	case quarantineUnknown:
		return "QuarantineUnknown"
	default:
		return "NotRequested"
	}
}

type quarantineFence interface{ Ensure(context.Context) error }
type quarantineTarget interface {
	Check() error
	Close() error
}

// revisionQuarantine contains faults in this process's original namespace only.
// It is not a persistent intent log or a restart/recovery protocol. Successful
// mutations never call it, and it has no runtime unfence/reset operation.
type revisionQuarantine struct {
	fence  quarantineFence
	target quarantineTarget
	state  atomic.Uint32
}

func newRevisionQuarantine() (*revisionQuarantine, error) {
	if !constants.IsTruthy(os.Getenv(constants.EnvExperimentalRevisionRuntime)) || !constants.ModeUsesNft(parseMode()) {
		return nil, nil
	}
	target, err := quarantine.PinNamespace()
	if err != nil {
		return nil, fmt.Errorf("pin runtime quarantine namespace: %w", err)
	}
	return &revisionQuarantine{fence: quarantine.NewFence(), target: target}, nil
}
func (q *revisionQuarantine) contain() error {
	q.state.Store(uint32(quarantineUnknown))
	err := q.confirm()
	if err == nil {
		q.state.Store(uint32(quarantineConfirmed))
	}
	log.Warnf("experimental runtime recovery: packet isolation=%s; containment error=%v", packetQuarantineState(q.state.Load()), err)
	return err
}
func (q *revisionQuarantine) confirm() error {
	if q.target == nil {
		return fmt.Errorf("original runtime namespace is unavailable")
	}
	// Never install a whole-namespace fence in an unrelated namespace.
	if err := q.target.Check(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.fence.Ensure(ctx); err != nil {
		return err
	}
	return q.target.Check()
}
