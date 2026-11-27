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

// HealthGate keeps required MITM readiness pending until startup completes; the first down reason remains latched for the gate's lifetime.
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
	// Keep check and store under one lock so SetReady cannot race past a latched MarkDown.
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
