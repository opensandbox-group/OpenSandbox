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

package mitmproxy

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// HealthGate: /healthz stays 503 until MarkStackReady when transparent mitm is required (env enabled).
//
// The fast-sandbox profile adds the addon-down latch (OSEP-0022 pinned CA):
// MarkDown records a permanent addon failure (missing or unloadable pinned
// CA, or the startup consistency check failing) — /healthz keeps returning
// 503 with an explicit reason while the rest of egress keeps serving policy
// actions. First failure wins: the down state is a startup latch, only a
// process restart can clear it. The pinned CA fingerprint rides along so
// /healthz and the actions status payload can mirror it.
type HealthGate struct {
	required bool
	ready    atomic.Bool

	mu         sync.Mutex
	downReason string
	caFp       string
}

func NewHealthGate() *HealthGate {
	required := constants.IsTruthy(os.Getenv(constants.EnvMitmproxyTransparent))
	g := &HealthGate{required: required}
	if !required {
		g.ready.Store(true)
	}
	return g
}

func (g *HealthGate) MarkStackReady() {
	g.SetReady(true)
}

func (g *HealthGate) SetReady(v bool) {
	if g == nil {
		return
	}
	// Hold the lock across the check AND the store: SetReady(true) races
	// MarkDown from the watcher goroutine (mitmproxy_transparent.go), and a
	// check-then-act split could interleave into ready:true over a latched
	// down state. The addon-down latch makes SetReady(true) a no-op — only a
	// fresh process (new gate) clears the latch.
	g.mu.Lock()
	defer g.mu.Unlock()
	if v && g.downReason != "" {
		return
	}
	g.ready.Store(v)
}

func (g *HealthGate) MitmPending() bool {
	if g == nil {
		return false
	}
	return g.required && !g.ready.Load()
}

// MarkDown latches the addon-down state and unsets readiness. The first
// reason wins (later restart attempts cannot clear it). The latch write and
// the readiness store complete under the same lock as SetReady's check, so a
// concurrent SetReady(true) can never interleave into ready:true over a
// latched down state.
func (g *HealthGate) MarkDown(reason string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.downReason == "" {
		g.downReason = reason
	}
	g.ready.Store(false)
}

// DownReason returns the latched addon-down reason and whether the addon is down.
func (g *HealthGate) DownReason() (string, bool) {
	if g == nil {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.downReason, g.downReason != ""
}

// SetCAFingerprint records the pinned CA fingerprint for the observability
// surfaces (/healthz, actions status payload). Empty = none provisioned.
func (g *HealthGate) SetCAFingerprint(fp string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.caFp = fp
	g.mu.Unlock()
}

// CAFingerprint returns the pinned CA fingerprint ("" when none).
func (g *HealthGate) CAFingerprint() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.caFp
}

// WaitReady polls until the gate is ready, ctx is cancelled, or 30s elapses.
func (g *HealthGate) WaitReady(ctx context.Context) bool {
	deadline := time.After(30 * time.Second)
	for g.MitmPending() {
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
	return true
}
