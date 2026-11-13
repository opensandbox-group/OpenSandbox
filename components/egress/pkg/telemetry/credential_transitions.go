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

package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Closed transition values for RecordCredentialVaultTransition. One value is
// one worker IPC call (a lifecycle command, a snapshot Apply or a Reconcile),
// not a deduplicated user operation.
const (
	CredentialTransitionRegister = "register"
	CredentialTransitionInstall  = "install"
	CredentialTransitionSuspend  = "suspend"
	CredentialTransitionUnload   = "unload"
	// Register and snapshot install rebuilt for a newly published child.
	CredentialTransitionReplay = "replay"
)

// Closed result values, classified from structured protocol state only.
const (
	CredentialTransitionOK            = "ok"
	CredentialTransitionRejected      = "rejected"
	CredentialTransitionIndeterminate = "indeterminate"
	CredentialTransitionFailed        = "failed"
)

// CredentialTransitionUnknown replaces any value outside the closed sets. The
// caller's string is never exported; normal execution paths never produce it.
const CredentialTransitionUnknown = "unknown"

func normalizeCredentialTransition(transition string) string {
	switch transition {
	case CredentialTransitionRegister, CredentialTransitionInstall, CredentialTransitionSuspend,
		CredentialTransitionUnload, CredentialTransitionReplay:
		return transition
	default:
		return CredentialTransitionUnknown
	}
}

func normalizeCredentialTransitionResult(result string) string {
	switch result {
	case CredentialTransitionOK, CredentialTransitionRejected, CredentialTransitionIndeterminate, CredentialTransitionFailed:
		return result
	default:
		return CredentialTransitionUnknown
	}
}

// RecordCredentialVaultTransition records one completed worker IPC call. The
// counter and the duration histogram always receive the same observation: a
// negative duration drops both (time.Duration cannot be non-finite), and
// missing instruments make it a no-op. It is best-effort observation, not
// evidence of isolation, drained connections or credential injection.
func RecordCredentialVaultTransition(transition, result string, elapsed time.Duration) {
	if credentialVaultTransitions == nil || credentialVaultTransitionDur == nil || elapsed < 0 {
		return
	}
	shared := egressSharedAttrs()
	attrs := make([]attribute.KeyValue, 0, len(shared)+2)
	attrs = append(attrs, shared...)
	attrs = append(attrs,
		attribute.String("transition", normalizeCredentialTransition(transition)),
		attribute.String("result", normalizeCredentialTransitionResult(result)))
	opt := metric.WithAttributes(attrs...)
	credentialVaultTransitions.Add(context.Background(), 1, opt)
	credentialVaultTransitionDur.Record(context.Background(), elapsed.Seconds(), opt)
}
