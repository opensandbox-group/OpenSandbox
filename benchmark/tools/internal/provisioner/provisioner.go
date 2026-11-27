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
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// RunnerServiceAccountName is the ServiceAccount provisioned in benchNamespace for the Runner Pod.
const RunnerServiceAccountName = "opensandbox-bench-runner"

type Options struct {
	Kubeconfig   string
	PrepareNodes bool
	Client       kubernetes.Interface
	RestConfig   *rest.Config
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
	reg.MustRegister(NewOverlayBDProvisioner())
	return reg
}

// OverlayBDProvisioner implements Phase 1 full-stack provisioning, preflight,
// and teardown for the fast-sandbox OverlayBD (OCI) + ResourceAware stack.
type OverlayBDProvisioner struct {
	DynamicClient dynamic.Interface
	CmdRunner     CommandRunner
	PodExecer     PodExecer
	LogReader     PodLogReader
	PollInterval  time.Duration
	ReadyTimeout  time.Duration
	HelmRelease   string
}

// NewOverlayBDProvisioner constructs a default OverlayBDProvisioner.
func NewOverlayBDProvisioner() *OverlayBDProvisioner {
	return &OverlayBDProvisioner{
		CmdRunner:    execCommandRunner{},
		PollInterval: defaultPollInterval,
		ReadyTimeout: defaultReadyTimeout,
		HelmRelease:  defaultHelmReleaseName,
	}
}

func (p *OverlayBDProvisioner) Name() string {
	return profile.RuntimeFastSandboxOverlayBD
}
