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

package suite

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// OperationSample captures per-operation telemetry used for latency distributions
// and automated sampled-trace drill-down (OSEP-0034 §5.4).
type OperationSample struct {
	Operation string
	SandboxID string
	TraceID   string
	Sampled   bool
	LatencyMs float64
	Status    string // ok | error
	AckState  string // Running | Pending
	ErrorCode string
}

// RunContext carries shared dependencies injected into every benchmark suite.
type RunContext struct {
	RunID      string
	Profile    *profile.BenchmarkProfile
	SDKConfig  opensandbox.ConnectionConfig
	Logger     *slog.Logger
	OutputDir  string
	WarmupRuns int
}

// Suite is the extension point for benchmark workloads.
// Implement this interface and register it in a Registry to add new scenarios
// (e.g., Phase 2 native S3/DART, classic K8s, Docker, or reclaim suites).
type Suite interface {
	Name() string
	Description() string
	SupportedRuntimes() []string // empty means all runtimes
	Run(ctx context.Context, rc RunContext) (*report.SuiteResult, []OperationSample, error)
}

// Registry stores named Suite implementations in a concurrency-safe map.
type Registry struct {
	mu     sync.RWMutex
	suites map[string]Suite
}

func NewRegistry() *Registry {
	return &Registry{suites: make(map[string]Suite)}
}

func (r *Registry) Register(s Suite) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s == nil || s.Name() == "" {
		return fmt.Errorf("suite must have a non-empty name")
	}
	if _, exists := r.suites[s.Name()]; exists {
		return fmt.Errorf("suite %q already registered", s.Name())
	}
	r.suites[s.Name()] = s
	return nil
}

func (r *Registry) MustRegister(s Suite) {
	if err := r.Register(s); err != nil {
		panic(err)
	}
}

func (r *Registry) Get(name string) (Suite, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.suites[name]
	return s, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.suites))
	for name := range r.suites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve returns the ordered slice of suites matching selector ("all" or comma-separated names)
// filtered to those supporting the profile's runtime.
func (r *Registry) Resolve(names []string, runtimeName string) ([]Suite, error) {
	if len(names) == 0 || (len(names) == 1 && names[0] == "all") {
		names = r.Names()
	}
	var out []Suite
	for _, name := range names {
		s, ok := r.Get(name)
		if !ok {
			return nil, fmt.Errorf("unknown benchmark suite %q (available: %v)", name, r.Names())
		}
		if !supportsRuntime(s, runtimeName) {
			return nil, fmt.Errorf("suite %q does not support runtime %q", name, runtimeName)
		}
		out = append(out, s)
	}
	return out, nil
}

func supportsRuntime(s Suite, runtimeName string) bool {
	supported := s.SupportedRuntimes()
	if len(supported) == 0 || runtimeName == "" {
		return true
	}
	for _, rt := range supported {
		if rt == runtimeName {
			return true
		}
	}
	return false
}
