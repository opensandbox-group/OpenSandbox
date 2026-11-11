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

package sandbox

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	fastpathv2 "github.com/alibaba/opensandbox/ingress/pkg/fastpath/v2"
)

// SandboxPhase is the ingress-facing classification of one fast-sandbox
// lifecycle probe (OSEP-0024 wake-on-access).
type SandboxPhase uint8

const (
	// SandboxPhaseReady means runtime and data plane are Ready; the request
	// is served through the normal resolution path.
	SandboxPhaseReady SandboxPhase = iota
	// SandboxPhaseWakeable means the sandbox is Pausing, Paused, or Resuming;
	// the request parks until the restore converges.
	SandboxPhaseWakeable
	// SandboxPhaseTerminal means the sandbox is stopping, stopped, or an
	// expired retained CR; requests fail fast with 404 and never resume.
	SandboxPhaseTerminal
	// SandboxPhaseNotReady covers every other transient state; requests keep
	// today's not-ready behavior (503).
	SandboxPhaseNotReady
)

func (p SandboxPhase) String() string {
	switch p {
	case SandboxPhaseReady:
		return "ready"
	case SandboxPhaseWakeable:
		return "wakeable"
	case SandboxPhaseTerminal:
		return "terminal"
	default:
		return "not_ready"
	}
}

// SandboxProbe is the result of one GetSandbox lifecycle inspection.
type SandboxProbe struct {
	Phase          SandboxPhase
	RuntimeState   fastpathv2.RuntimeState
	DataPlaneState fastpathv2.DataPlaneState
	CheckpointID   string
}

// ResumeOutcome classifies one ResumeSandbox call.
type ResumeOutcome uint8

const (
	// ResumeAccepted means this caller flipped the desired state to Running.
	ResumeAccepted ResumeOutcome = iota
	// ResumeAlreadyRunning means someone else already resumed; join the
	// in-flight restore. Ambiguous with an empty checkpoint fence — the
	// same outcome fires when the pause is still in progress — so callers
	// must re-probe before treating it as a join.
	ResumeAlreadyRunning
	// ResumeConflict means the checkpoint fence fired (a re-pause raced
	// in); re-probe and restart with the fresh checkpoint.
	ResumeConflict
)

// SandboxLifecycle is implemented by providers with authority to inspect
// fast-sandbox desired state and flip it back to Running (OSEP-0024
// wake-on-access).
type SandboxLifecycle interface {
	// ProbeSandbox inspects the sandbox lifecycle state via FastPath GetSandbox.
	ProbeSandbox(ctx context.Context, target EndpointTarget) (SandboxProbe, error)
	// ResumeSandbox flips the desired state back to Running, asynchronously:
	// convergence is observed through ProbeSandbox. request_id must be
	// unique per checkpoint intent, or FastPath-side dedup can replay a
	// rejected attempt's outcome.
	ResumeSandbox(ctx context.Context, target EndpointTarget, expectedCheckpointID, requestID string) (ResumeOutcome, error)
}

// ErrLifecycleUnavailable is returned when the provider has no lifecycle RPC
// surface (resolver-only construction, nothing injected).
var ErrLifecycleUnavailable = errors.New("FastPath lifecycle RPCs are not configured on this provider")

// ErrSandboxLifecycleRejected indicates FastPath permanently rejected the
// lifecycle call (auth, validation, unimplemented RPC, server fault):
// retrying within a park budget cannot succeed.
var ErrSandboxLifecycleRejected = errors.New("FastPath permanently rejected the lifecycle request")

// permanentLifecycleCodes will not heal within a park budget.
var permanentLifecycleCodes = map[codes.Code]bool{
	codes.PermissionDenied: true,
	codes.Unauthenticated:  true,
	codes.InvalidArgument:  true,
	codes.Unimplemented:    true,
	codes.Internal:         true,
}

// ProbeSandbox inspects the sandbox lifecycle state over the provider's
// existing FastPath connection.
func (p *FastSandboxProvider) ProbeSandbox(ctx context.Context, target EndpointTarget) (SandboxProbe, error) {
	if p.lifecycleRPC == nil {
		return SandboxProbe{}, ErrLifecycleUnavailable
	}
	if err := validateLifecycleTarget(target); err != nil {
		return SandboxProbe{}, err
	}
	request := &fastpathv2.GetSandboxRequest{
		Sandbox: namespacedSandboxReference(target),
	}
	rpcCtx, cancel := context.WithTimeout(ctx, p.waitTimeout)
	defer cancel()
	response, err := p.lifecycleRPC.GetSandbox(rpcCtx, request)
	if err != nil {
		return SandboxProbe{}, mapFastPathLifecycleError(err)
	}
	return ClassifySandboxInfo(response.GetSandbox()), nil
}

// ResumeSandbox submits an asynchronous resume intent over the provider's
// existing FastPath connection.
func (p *FastSandboxProvider) ResumeSandbox(ctx context.Context, target EndpointTarget, expectedCheckpointID, requestID string) (ResumeOutcome, error) {
	if p.lifecycleRPC == nil {
		return 0, ErrLifecycleUnavailable
	}
	if err := validateLifecycleTarget(target); err != nil {
		return 0, err
	}
	request := &fastpathv2.ResumeSandboxRequest{
		RequestId:            requestID,
		Sandbox:              namespacedSandboxReference(target),
		ExpectedCheckpointId: expectedCheckpointID,
	}
	rpcCtx, cancel := context.WithTimeout(ctx, p.waitTimeout)
	defer cancel()
	if _, err := p.lifecycleRPC.ResumeSandbox(rpcCtx, request); err != nil {
		switch {
		case permanentLifecycleCodes[status.Code(err)]:
			return 0, &fastPathResolutionError{
				public: fmt.Errorf("%w: %s", ErrSandboxLifecycleRejected, status.Code(err)),
				cause:  err,
			}
		case status.Code(err) == codes.NotFound:
			return 0, &fastPathResolutionError{
				public: fmt.Errorf("%w: sandbox not found", ErrSandboxNotFound),
				cause:  err,
			}
		case status.Code(err) == codes.FailedPrecondition:
			// "not paused": another actor already resumed; join instead of failing.
			return ResumeAlreadyRunning, nil
		case status.Code(err) == codes.Aborted:
			// Checkpoint fence: a re-pause raced in; restart within the budget.
			return ResumeConflict, nil
		case status.Code(err) == codes.Canceled:
			// Usually the request context: a client disconnect mid-wake.
			// Wrapping context.Canceled lets the proxy suppress the answer.
			return 0, &fastPathResolutionError{
				public: fmt.Errorf("%w: FastPath resume canceled", context.Canceled),
				cause:  err,
			}
		default:
			return 0, &fastPathResolutionError{
				public: fmt.Errorf("%w: FastPath resume temporarily unavailable", ErrSandboxNotReady),
				cause:  err,
			}
		}
	}
	return ResumeAccepted, nil
}

// ClassifySandboxInfo maps a FastPath SandboxInfo onto the ingress-facing
// lifecycle phases. Ready requires both runtime and data plane Ready; the
// paused family (Pausing/Paused/Resuming) is uniformly wakeable; stopping,
// stopped, and expired retained CRs are terminal.
func ClassifySandboxInfo(info *fastpathv2.SandboxInfo) SandboxProbe {
	probe := SandboxProbe{
		RuntimeState:   info.GetRuntime().GetState(),
		DataPlaneState: info.GetDataPlane().GetState(),
		CheckpointID:   info.GetCheckpoint().GetCheckpointId(),
	}
	switch probe.RuntimeState {
	case fastpathv2.RuntimeState_RUNTIME_STATE_READY:
		if probe.DataPlaneState == fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY {
			probe.Phase = SandboxPhaseReady
		} else {
			probe.Phase = SandboxPhaseNotReady
		}
	case fastpathv2.RuntimeState_RUNTIME_STATE_PAUSING,
		fastpathv2.RuntimeState_RUNTIME_STATE_PAUSED,
		fastpathv2.RuntimeState_RUNTIME_STATE_RESUMING:
		probe.Phase = SandboxPhaseWakeable
	case fastpathv2.RuntimeState_RUNTIME_STATE_STOPPING,
		fastpathv2.RuntimeState_RUNTIME_STATE_STOPPED:
		probe.Phase = SandboxPhaseTerminal
	default:
		probe.Phase = SandboxPhaseNotReady
	}
	return probe
}

func mapFastPathLifecycleError(err error) error {
	var public error
	switch {
	case permanentLifecycleCodes[status.Code(err)]:
		public = fmt.Errorf("%w: %s", ErrSandboxLifecycleRejected, status.Code(err))
	case status.Code(err) == codes.NotFound:
		public = fmt.Errorf("%w: sandbox not found", ErrSandboxNotFound)
	case status.Code(err) == codes.Canceled:
		// Usually the request context: a client disconnect mid-probe.
		public = fmt.Errorf("%w: FastPath lifecycle canceled", context.Canceled)
	case status.Code(err) == codes.Unavailable, status.Code(err) == codes.DeadlineExceeded,
		status.Code(err) == codes.ResourceExhausted, status.Code(err) == codes.FailedPrecondition:
		public = fmt.Errorf("%w: FastPath lifecycle temporarily unavailable", ErrSandboxNotReady)
	default:
		public = fmt.Errorf("%w: FastPath lifecycle failed: %s", ErrSandboxNotReady, status.Code(err))
	}
	return &fastPathResolutionError{public: public, cause: err}
}

func validateLifecycleTarget(target EndpointTarget) error {
	if target.Namespace == "" || target.SandboxID == "" {
		return errors.New("Fast Sandbox lifecycle target requires namespace and sandbox ID")
	}
	return nil
}

func namespacedSandboxReference(target EndpointTarget) *fastpathv2.SandboxReference {
	return &fastpathv2.SandboxReference{
		NamespacedName: &fastpathv2.NamespacedName{Namespace: target.Namespace, Name: target.SandboxID},
	}
}
