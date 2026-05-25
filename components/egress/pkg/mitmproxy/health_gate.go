// Copyright 2026 Alibaba Group Holding Ltd.
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
	"os"
	"sync/atomic"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

// HealthGate: /healthz stays 503 until MarkStackReady when transparent mitm is required (env enabled).
type HealthGate struct {
	required bool
	ready    atomic.Bool
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
	if g != nil {
		g.ready.Store(v)
	}
}

func (g *HealthGate) MitmPending() bool {
	if g == nil {
		return false
	}
	return g.required && !g.ready.Load()
}
