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

// Package idlepolicy parses the per-sandbox idle policy gating OSEP-0024
// wake-on-access, carried as the sandbox.opensandbox.io/idle-policy
// annotation on the fast-sandbox Sandbox CR. Parsing fails safe: anything
// invalid is treated as no policy.
package idlepolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// AnnotationKey carries the JSON-encoded idle policy on the Sandbox CR.
const AnnotationKey = "sandbox.opensandbox.io/idle-policy"

const (
	OnIdlePause           = "pause"
	MinIdleTimeoutSeconds = 30
	MaxIdleTimeoutSeconds = 86400
)

// Policy mirrors the create-time lifecycle.idlePolicy object.
type Policy struct {
	IdleTimeoutSeconds *int            `json:"idleTimeoutSeconds,omitempty"`
	OnIdle             string          `json:"onIdle,omitempty"`
	WakeOnAccess       *bool           `json:"wakeOnAccess,omitempty"`
	ActiveProbe        json.RawMessage `json:"activeProbe,omitempty"`
}

// Parse decodes and validates one annotation value; unknown fields,
// trailing data, and JSON null are rejected.
func Parse(raw string) (Policy, error) {
	// "null" decodes into the zero value without error and would otherwise
	// flip the gate open via the defaulting path — an implicit opt-in.
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return Policy{}, errors.New("idlepolicy: empty policy")
	}
	var policy Policy
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return Policy{}, fmt.Errorf("idlepolicy: decode %s: %w", AnnotationKey, err)
	}
	// Only trailing whitespace may follow the object.
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Policy{}, errors.New("idlepolicy: trailing data")
	}
	if policy.IdleTimeoutSeconds == nil {
		if policy.OnIdle != "" {
			return Policy{}, errors.New("idlepolicy: onIdle without idleTimeoutSeconds")
		}
	} else {
		if policy.OnIdle == "" {
			return Policy{}, errors.New("idlepolicy: idleTimeoutSeconds without onIdle")
		}
		if policy.OnIdle != OnIdlePause {
			return Policy{}, fmt.Errorf("idlepolicy: unsupported onIdle %q", policy.OnIdle)
		}
		if *policy.IdleTimeoutSeconds < MinIdleTimeoutSeconds || *policy.IdleTimeoutSeconds > MaxIdleTimeoutSeconds {
			return Policy{}, fmt.Errorf("idlepolicy: idleTimeoutSeconds %d outside [%d, %d]",
				*policy.IdleTimeoutSeconds, MinIdleTimeoutSeconds, MaxIdleTimeoutSeconds)
		}
	}
	return policy, nil
}

// WakeAllowedOf reports whether raw permits wake-on-access; every parse
// failure resolves to false. wakeOnAccess defaults to true.
func WakeAllowedOf(raw string) bool {
	policy, err := Parse(raw)
	if err != nil {
		return false
	}
	return policy.WakeOnAccess == nil || *policy.WakeOnAccess
}
