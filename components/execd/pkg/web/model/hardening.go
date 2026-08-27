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

package model

// HardeningLayerState reports whether one hardening layer is actually
// enforced (OSEP-0018 §6).
type HardeningLayerState struct {
	// State is "active" | "disabled" (not configured) | "degraded"
	// (configured but a prerequisite is missing) | "unsupported".
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// HardeningStatus reports which execd init-mode controls are in effect
// (OSEP-0018). This is execd-global state (execd as the sandbox init / PID 1),
// not an isolation/bwrap capability; it is reported on the capabilities
// endpoint so operators see what is actually enforced in one place.
type HardeningStatus struct {
	InitMode     string               `json:"init_mode"`     // "pid1" | "subreaper" | "none"
	SignalShield bool                 `json:"signal_shield"` // kernel PID 1 signal shield active
	CapDrop      *HardeningLayerState `json:"cap_drop"`      // bounding-set/capability reduction
	Seccomp      *HardeningLayerState `json:"seccomp"`       // seccomp floor on user code
	Landlock     *HardeningLayerState `json:"landlock"`      // filesystem confinement on user code
	Ebpf         *HardeningLayerState `json:"ebpf"`          // eBPF exec/connect/privilege observation
}
