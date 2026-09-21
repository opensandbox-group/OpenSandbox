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

package model

import (
	"github.com/alibaba/opensandbox/execd/pkg/lifecycle"
)

// RuntimeInitRequest carries the sandbox-scoped parameters applied by
// POST /internal/init. Container templates only keep configuration that does not
// change during the container lifetime; everything tied to a sandbox
// allocation arrives here. Calls perform complete initialization by default;
// snapshot restore callers may explicitly preserve the existing runtime state.
type RuntimeInitRequest struct {
	// SandboxID is the authoritative sandbox identity, replacing any
	// OPENSANDBOX_ID injected into the container environment.
	SandboxID string `json:"sandboxId"`

	// Generation is the control-plane-assigned allocation counter. It is
	// reported on /ready and in metrics. Execd treats it as an opaque binding
	// identity and does not compare it monotonically.
	Generation uint64 `json:"generation"`

	// EntrypointPolicy controls what happens to the user entrypoint:
	//   "keep" (default): never start or restart it. A running entrypoint
	//     (legacy fallback path) is adopted as-is — signal forwarding and
	//     the container exit code stay with it, but it keeps the template
	//     env. In gated mode nothing starts (API-only sandbox); a warning
	//     is returned when a template entrypoint was suppressed.
	//   "restart" (init mode only): retire the running entrypoint and start
	//     a fresh one with the RuntimeBinding env. In classic mode execd
	//     does not own the entrypoint, so a restart request is ignored with
	//     a warning.
	EntrypointPolicy string `json:"entrypointPolicy,omitempty"`

	// AccessTokenHash is "sha256:<hex>" of the raw execd API token. The raw
	// token never reaches execd; after apply, API auth verifies request
	// tokens against this hash.
	AccessTokenHash string `json:"accessTokenHash,omitempty"`

	// Envs are the sandbox-level user envs, applied with replace semantics:
	// keys omitted from a later /internal/init are removed.
	Envs map[string]string `json:"envs,omitempty"`

	// Lifecycle replaces the creation-time lifecycle configuration
	// (preStart runs before the entrypoint; periodic hooks replace the
	// previous generation's hooks). Omitted means: keep the template-level
	// lifecycle. PreserveRuntimeState leaves the active lifecycle untouched.
	Lifecycle *lifecycle.Config `json:"lifecycle,omitempty"`

	// Telemetry carries dynamic observability attributes (tenant, plan...)
	// attached to metrics alongside sandbox_id/generation.
	Telemetry *RuntimeInitTelemetry `json:"telemetry,omitempty"`

	// PreserveRuntimeState skips the destructive runtime reset and only
	// replaces the sandbox-scoped binding. Control planes restoring a runtime
	// snapshot set this explicitly so restored processes and sessions survive.
	// The default false value performs a complete initialization.
	PreserveRuntimeState bool `json:"preserveRuntimeState,omitempty"`
}

// RuntimeInitTelemetry groups the observability part of the request.
type RuntimeInitTelemetry struct {
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Entrypoint policies for RuntimeInitRequest.EntrypointPolicy.
const (
	// EntrypointPolicyKeep (default) never starts or restarts the user
	// entrypoint.
	EntrypointPolicyKeep = "keep"
	// EntrypointPolicyRestart retires a running entrypoint and starts a
	// fresh one (init mode only).
	EntrypointPolicyRestart = "restart"
)

// RuntimeInitResponse acknowledges an applied RuntimeBinding.
type RuntimeInitResponse struct {
	Status     string   `json:"status"`
	SandboxID  string   `json:"sandboxId"`
	Generation uint64   `json:"generation"`
	Warning    []string `json:"warning,omitempty"`
}

// RuntimeReadyResponse reports runtime-init readiness on GET /ready.
type RuntimeReadyResponse struct {
	Initialized bool   `json:"initialized"`
	SandboxID   string `json:"sandboxId,omitempty"`
	Generation  uint64 `json:"generation,omitempty"`
}

// RuntimeInitStatus advertises the runtime-init protocol version on the
// capabilities endpoint so control planes can detect support.
type RuntimeInitStatus struct {
	Version int `json:"version"`
}
