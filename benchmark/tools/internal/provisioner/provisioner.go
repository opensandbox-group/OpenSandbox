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

package provisioner

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"k8s.io/client-go/kubernetes"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

type Options struct {
	Kubeconfig   string
	PrepareNodes bool
	Client       kubernetes.Interface
	Logger       *slog.Logger
}

// Provisioner abstracts runtime-specific cluster preflight, full-stack bring-up,
// and teardown so Phase 2 runtimes (Native S3/DART, classic K8s, Docker) can plug in cleanly.
type Provisioner interface {
	Name() string
	Preflight(ctx context.Context, prof *profile.BenchmarkProfile, opts Options) (*report.EnvironmentFingerprint, error)
	Up(ctx context.Context, prof *profile.BenchmarkProfile, opts Options) error
	Down(ctx context.Context, prof *profile.BenchmarkProfile, opts Options) (*report.LeakAudit, error)
}

type Registry struct {
	mu           sync.RWMutex
	provisioners map[string]Provisioner
}

func NewRegistry() *Registry {
	return &Registry{provisioners: make(map[string]Provisioner)}
}

func (r *Registry) Register(p Provisioner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p == nil || p.Name() == "" {
		return fmt.Errorf("provisioner must have a non-empty name")
	}
	if _, exists := r.provisioners[p.Name()]; exists {
		return fmt.Errorf("provisioner %q already registered", p.Name())
	}
	r.provisioners[p.Name()] = p
	return nil
}

func (r *Registry) MustRegister(p Provisioner) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

func (r *Registry) Get(name string) (Provisioner, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.provisioners[name]
	if !ok {
		return nil, fmt.Errorf("unknown runtime provisioner %q (available: %v)", name, r.Names())
	}
	return p, nil
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.provisioners))
	for name := range r.provisioners {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DefaultRegistry registers the Phase 1 fast-sandbox OverlayBD provisioner.
func DefaultRegistry() *Registry {
	reg := NewRegistry()
	reg.MustRegister(&overlayBDProvisioner{})
	return reg
}

type overlayBDProvisioner struct{}

func (p *overlayBDProvisioner) Name() string {
	return profile.RuntimeFastSandboxOverlayBD
}

func (p *overlayBDProvisioner) Preflight(_ context.Context, prof *profile.BenchmarkProfile, _ Options) (*report.EnvironmentFingerprint, error) {
	return nil, fmt.Errorf("preflight for runtime %q (profile %q) is scaffolded and not yet implemented", p.Name(), prof.Metadata.Name)
}

func (p *overlayBDProvisioner) Up(_ context.Context, prof *profile.BenchmarkProfile, _ Options) error {
	return fmt.Errorf("env up for runtime %q (profile %q) is scaffolded and not yet implemented", p.Name(), prof.Metadata.Name)
}

func (p *overlayBDProvisioner) Down(_ context.Context, prof *profile.BenchmarkProfile, _ Options) (*report.LeakAudit, error) {
	return nil, fmt.Errorf("env down for runtime %q (profile %q) is scaffolded and not yet implemented", p.Name(), prof.Metadata.Name)
}
