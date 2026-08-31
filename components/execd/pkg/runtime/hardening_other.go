//go:build !linux

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

// Hardening is Linux-only (OSEP-0018); on other platforms it is a no-op.

package runtime

import (
	"sync/atomic"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
)

// Requested-state flags so the capabilities endpoint can distinguish a
// configured layer that this platform cannot provide ("unsupported") from
// an opt-out deployment ("disabled").
var (
	otherHardeningRequested atomic.Bool
	otherLandlockRequested  atomic.Bool
	otherEbpfRequested      atomic.Bool
)

// InitHardening is a no-op off Linux; it only records which layers were
// requested so ReportHardening can report them as unsupported instead of
// silently claiming they are disabled.
func InitHardening(cfg isolation.Config) error {
	otherHardeningRequested.Store(cfg.Hardening != nil && cfg.Hardening.Enabled)
	otherLandlockRequested.Store(cfg.Landlock != nil && cfg.Landlock.Enabled)
	return nil
}

// SetEbpfState records whether eBPF observation was requested off Linux.
func SetEbpfState(state LayerState) {
	otherEbpfRequested.Store(state.State != "disabled")
}

// HardeningReport reports that no hardening layer is in effect, marking
// configured layers as unsupported rather than disabled.
func ReportHardening() HardeningReport {
	initMode, shield := InitModeReport()
	report := HardeningReport{
		InitMode:     initMode,
		SignalShield: shield,
		CapDrop:      LayerState{State: "disabled", Message: "hardening is Linux-only"},
		Seccomp:      LayerState{State: "disabled", Message: "hardening is Linux-only"},
		Landlock:     LayerState{State: "disabled", Message: "hardening is Linux-only"},
		Ebpf:         LayerState{State: "disabled", Message: "hardening is Linux-only"},
	}
	if otherHardeningRequested.Load() {
		msg := "hardening requested but unavailable on this platform (Linux-only)"
		report.CapDrop = LayerState{State: "unsupported", Message: msg}
		report.Seccomp = LayerState{State: "unsupported", Message: msg}
	}
	if otherLandlockRequested.Load() {
		report.Landlock = LayerState{State: "unsupported",
			Message: "landlock requested but unavailable on this platform (Linux-only)"}
	}
	if otherEbpfRequested.Load() {
		report.Ebpf = LayerState{State: "unsupported",
			Message: "eBPF observation requested but unavailable on this platform (Linux-only)"}
	}
	return report
}
