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

	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/profile"
	"github.com/opensandbox-group/OpenSandbox/benchmark/tools/internal/report"
)

// DefaultRegistry returns a Registry pre-populated with all Phase 1 OverlayBD suites.
func DefaultRegistry() *Registry {
	reg := NewRegistry()
	reg.MustRegister(&scaffoldSuite{
		name:        "schedule-burst",
		description: "Stepped concurrency waves of create -> ping -> first-exec -> delete",
		runtimes:    []string{profile.RuntimeFastSandboxOverlayBD},
	})
	reg.MustRegister(&scaffoldSuite{
		name:        "steady-churn",
		description: "Sustained target-RPS create -> short workload -> delete churn",
		runtimes:    []string{profile.RuntimeFastSandboxOverlayBD},
	})
	reg.MustRegister(&scaffoldSuite{
		name:        "pause-resume",
		description: "Full checkpoint pause/resume matrix (warm, cache-evicted, cross-node, dirty-upper, 24-layer squash)",
		runtimes:    []string{profile.RuntimeFastSandboxOverlayBD},
	})
	reg.MustRegister(&scaffoldSuite{
		name:        "rootfs-only-restore",
		description: "RootfsOnly snapshot create/fork across warm/cold materialize, fan-out, cross-node, and OSEP-0028 overrides",
		runtimes:    []string{profile.RuntimeFastSandboxOverlayBD},
	})
	reg.MustRegister(&scaffoldSuite{
		name:        "wake-storm",
		description: "OSEP-0024 auto-pause + concurrent ingress fan-out and singleflight wake bursts",
		runtimes:    []string{profile.RuntimeFastSandboxOverlayBD},
	})
	reg.MustRegister(&scaffoldSuite{
		name:        "high-density-soak",
		description: "Multi-tier density oversell soak with host/fastlet/egress/proxy load sampling and drain recovery",
		runtimes:    []string{profile.RuntimeFastSandboxOverlayBD},
	})
	return reg
}

type scaffoldSuite struct {
	name        string
	description string
	runtimes    []string
}

func (s *scaffoldSuite) Name() string                { return s.name }
func (s *scaffoldSuite) Description() string         { return s.description }
func (s *scaffoldSuite) SupportedRuntimes() []string { return s.runtimes }

func (s *scaffoldSuite) Run(_ context.Context, _ RunContext) (*report.SuiteResult, []OperationSample, error) {
	return nil, nil, fmt.Errorf("suite %q workload executor is scaffolded and not yet implemented", s.name)
}
