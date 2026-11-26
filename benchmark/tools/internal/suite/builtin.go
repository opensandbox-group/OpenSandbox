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

// DefaultRegistry returns a Registry pre-populated with all 6 Phase 1 OverlayBD suites.
// Cluster-level hooks can be injected dynamically per run via WithBenchHooks(ctx, hooks),
// or statically via NewDefaultRegistryWithHooks(hooks).
func DefaultRegistry() *Registry {
	return NewDefaultRegistryWithHooks(BenchHooks{})
}

// NewDefaultRegistryWithHooks constructs a Registry with all 6 Phase 1 OverlayBD suites
// pre-wired with the supplied BenchHooks.
func NewDefaultRegistryWithHooks(hooks BenchHooks) *Registry {
	reg := NewRegistry()

	var lifecycleOpts []SuiteOption
	if hooks.CacheEvictor != nil || hooks.NodeIsolator != nil || hooks.NodeResolver != nil || hooks.MetricsSnapshotter != nil || hooks.TemplateID != "" {
		lifecycleOpts = append(lifecycleOpts, WithSuiteHooks(hooks))
	}
	reg.MustRegister(NewScheduleBurstSuite(lifecycleOpts...))
	reg.MustRegister(NewSteadyChurnSuite(lifecycleOpts...))
	reg.MustRegister(NewPauseResumeSuite(lifecycleOpts...))

	var rootfsOpts []RootfsOnlyRestoreOption
	if hooks.NodeIsolator != nil {
		rootfsOpts = append(rootfsOpts, WithRootfsNodeIsolator(hooks.NodeIsolator.IsolateNodeFastlets))
	}
	reg.MustRegister(NewRootfsOnlyRestoreSuite(rootfsOpts...))

	var wakeOpts []WakeStormOption
	if hooks.WakeResumeCounter != nil {
		wakeOpts = append(wakeOpts, WithWakeResumeCounter(hooks.WakeResumeCounter))
	}
	reg.MustRegister(NewWakeStormSuite(wakeOpts...))

	var soakOpts []HighDensitySoakOption
	if hooks.SoakLoadSampler != nil {
		soakOpts = append(soakOpts, WithSoakLoadSampler(hooks.SoakLoadSampler))
	}
	if hooks.SoakNodeMemoryMiB > 0 {
		soakOpts = append(soakOpts, WithSoakNodeMemoryMiB(hooks.SoakNodeMemoryMiB))
	}
	reg.MustRegister(NewHighDensitySoakSuite(soakOpts...))

	return reg
}
