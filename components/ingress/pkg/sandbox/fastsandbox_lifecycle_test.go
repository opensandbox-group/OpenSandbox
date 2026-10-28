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
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	fastpathv2 "github.com/alibaba/opensandbox/ingress/pkg/fastpath/v2"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type scriptedLifecycleRPCs struct {
	getSandboxFunc func(context.Context, *fastpathv2.GetSandboxRequest) (*fastpathv2.GetSandboxResponse, error)
	resumeFunc     func(context.Context, *fastpathv2.ResumeSandboxRequest) (*fastpathv2.ResumeSandboxResponse, error)

	getSandboxRequests []*fastpathv2.GetSandboxRequest
	resumeRequests     []*fastpathv2.ResumeSandboxRequest
}

func (s *scriptedLifecycleRPCs) GetSandbox(ctx context.Context, request *fastpathv2.GetSandboxRequest, _ ...grpc.CallOption) (*fastpathv2.GetSandboxResponse, error) {
	s.getSandboxRequests = append(s.getSandboxRequests, request)
	return s.getSandboxFunc(ctx, request)
}

func (s *scriptedLifecycleRPCs) ResumeSandbox(ctx context.Context, request *fastpathv2.ResumeSandboxRequest, _ ...grpc.CallOption) (*fastpathv2.ResumeSandboxResponse, error) {
	s.resumeRequests = append(s.resumeRequests, request)
	return s.resumeFunc(ctx, request)
}

func TestClassifySandboxInfoMapsLifecyclePhases(t *testing.T) {
	for _, test := range []struct {
		name      string
		runtime   fastpathv2.RuntimeState
		dataPlane fastpathv2.DataPlaneState
		expected  SandboxPhase
	}{
		{"ready", fastpathv2.RuntimeState_RUNTIME_STATE_READY, fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY, SandboxPhaseReady},
		{"runtime ready data plane publishing", fastpathv2.RuntimeState_RUNTIME_STATE_READY, fastpathv2.DataPlaneState_DATA_PLANE_STATE_PUBLISHING, SandboxPhaseNotReady},
		{"pausing", fastpathv2.RuntimeState_RUNTIME_STATE_PAUSING, fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY, SandboxPhaseWakeable},
		{"paused", fastpathv2.RuntimeState_RUNTIME_STATE_PAUSED, fastpathv2.DataPlaneState_DATA_PLANE_STATE_UNKNOWN, SandboxPhaseWakeable},
		{"resuming", fastpathv2.RuntimeState_RUNTIME_STATE_RESUMING, fastpathv2.DataPlaneState_DATA_PLANE_STATE_PENDING, SandboxPhaseWakeable},
		{"stopping", fastpathv2.RuntimeState_RUNTIME_STATE_STOPPING, fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY, SandboxPhaseTerminal},
		{"stopped", fastpathv2.RuntimeState_RUNTIME_STATE_STOPPED, fastpathv2.DataPlaneState_DATA_PLANE_STATE_UNKNOWN, SandboxPhaseTerminal},
		{"failed", fastpathv2.RuntimeState_RUNTIME_STATE_FAILED, fastpathv2.DataPlaneState_DATA_PLANE_STATE_READY, SandboxPhaseNotReady},
		{"creating", fastpathv2.RuntimeState_RUNTIME_STATE_CREATING, fastpathv2.DataPlaneState_DATA_PLANE_STATE_PENDING, SandboxPhaseNotReady},
		{"unspecified", fastpathv2.RuntimeState_RUNTIME_STATE_UNSPECIFIED, fastpathv2.DataPlaneState_DATA_PLANE_STATE_UNSPECIFIED, SandboxPhaseNotReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := ClassifySandboxInfo(&fastpathv2.SandboxInfo{
				Runtime:    &fastpathv2.RuntimeInfo{State: test.runtime},
				DataPlane:  &fastpathv2.DataPlaneInfo{State: test.dataPlane},
				Checkpoint: &fastpathv2.CheckpointInfo{CheckpointId: "ckpt-1"},
			})
			require.Equal(t, test.expected, probe.Phase)
			require.Equal(t, "ckpt-1", probe.CheckpointID)
		})
	}
}

func TestProbeSandboxMapsGRPCErrors(t *testing.T) {
	lifecycle := &scriptedLifecycleRPCs{getSandboxFunc: func(context.Context, *fastpathv2.GetSandboxRequest) (*fastpathv2.GetSandboxResponse, error) {
		return nil, status.Error(codes.NotFound, "no sandbox")
	}}
	provider, err := NewFastSandboxProviderWithLifecycle(&fakeFastPathResolver{now: time.Now()}, lifecycle, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
	require.NoError(t, err)
	_, err = provider.ProbeSandbox(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080})
	require.ErrorIs(t, err, ErrSandboxNotFound)

	lifecycle.getSandboxFunc = func(context.Context, *fastpathv2.GetSandboxRequest) (*fastpathv2.GetSandboxResponse, error) {
		return nil, status.Error(codes.Unavailable, "down")
	}
	_, err = provider.ProbeSandbox(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080})
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.NotContains(t, err.Error(), "down")

	_, err = provider.ProbeSandbox(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "", Port: 8080})
	require.ErrorContains(t, err, "requires namespace and sandbox ID")
}

func TestProbeSandboxWithoutLifecycleConfigurationFails(t *testing.T) {
	provider := NewFastSandboxProviderWithResolver(&fakeFastPathResolver{now: time.Now()}, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
	_, err := provider.ProbeSandbox(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080})
	require.ErrorIs(t, err, ErrLifecycleUnavailable)
	_, err = provider.ResumeSandbox(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080}, "", "req")
	require.ErrorIs(t, err, ErrLifecycleUnavailable)
}

func TestLifecycleCanceledWrapsContextCanceled(t *testing.T) {
	// The wake path probes and resumes on the request context; a client
	// disconnect surfaces as codes.Canceled. Wrapping context.Canceled lets
	// the proxy suppress the answer to the gone connection.
	lifecycle := &scriptedLifecycleRPCs{
		getSandboxFunc: func(context.Context, *fastpathv2.GetSandboxRequest) (*fastpathv2.GetSandboxResponse, error) {
			return nil, status.Error(codes.Canceled, "client disconnected")
		},
		resumeFunc: func(context.Context, *fastpathv2.ResumeSandboxRequest) (*fastpathv2.ResumeSandboxResponse, error) {
			return nil, status.Error(codes.Canceled, "client disconnected")
		},
	}
	provider, err := NewFastSandboxProviderWithLifecycle(&fakeFastPathResolver{now: time.Now()}, lifecycle, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
	require.NoError(t, err)
	target := EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080}

	_, err = provider.ProbeSandbox(context.Background(), target)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrSandboxNotReady)

	_, err = provider.ResumeSandbox(context.Background(), target, "ckpt", "req")
	require.ErrorIs(t, err, context.Canceled)
	detailed := err.(interface{ InternalCause() error })
	require.Error(t, detailed.InternalCause())
}

func TestResumeSandboxMapsFenceOutcomes(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		expected ResumeOutcome
		wantErr  error
	}{
		{"accepted", nil, ResumeAccepted, nil},
		{"already running joins", status.Error(codes.FailedPrecondition, "not paused"), ResumeAlreadyRunning, nil},
		{"checkpoint conflict restarts", status.Error(codes.Aborted, "checkpoint changed"), ResumeConflict, nil},
		{"not found", status.Error(codes.NotFound, "gone"), 0, ErrSandboxNotFound},
		{"permission denied is permanent", status.Error(codes.PermissionDenied, "rbac"), 0, ErrSandboxLifecycleRejected},
		{"unimplemented is permanent", status.Error(codes.Unimplemented, "old fastpath"), 0, ErrSandboxLifecycleRejected},
		{"internal is permanent", status.Error(codes.Internal, "bug"), 0, ErrSandboxLifecycleRejected},
		{"unavailable", status.Error(codes.Unavailable, "down"), 0, ErrSandboxNotReady},
	} {
		t.Run(test.name, func(t *testing.T) {
			lifecycle := &scriptedLifecycleRPCs{resumeFunc: func(context.Context, *fastpathv2.ResumeSandboxRequest) (*fastpathv2.ResumeSandboxResponse, error) {
				return &fastpathv2.ResumeSandboxResponse{}, test.err
			}}
			provider, err := NewFastSandboxProviderWithLifecycle(&fakeFastPathResolver{now: time.Now()}, lifecycle, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
			require.NoError(t, err)
			outcome, err := provider.ResumeSandbox(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080}, "ckpt-1", "wake-sb-1")
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, outcome)
			require.Equal(t, "ckpt-1", lifecycle.resumeRequests[0].GetExpectedCheckpointId())
			require.Equal(t, "wake-sb-1", lifecycle.resumeRequests[0].GetRequestId())
		})
	}
}

func TestProbeUnknownErrorCodeClassifiesAsNotReady(t *testing.T) {
	// Unknown codes keep the retryable classification but carry the
	// sentinel so errors.Is can tell them apart from permanent rejections.
	lifecycle := &scriptedLifecycleRPCs{getSandboxFunc: func(context.Context, *fastpathv2.GetSandboxRequest) (*fastpathv2.GetSandboxResponse, error) {
		return nil, status.Error(codes.DataLoss, "???")
	}}
	provider, err := NewFastSandboxProviderWithLifecycle(&fakeFastPathResolver{now: time.Now()}, lifecycle, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
	require.NoError(t, err)
	_, err = provider.ProbeSandbox(context.Background(), EndpointTarget{Namespace: "tenant-a", SandboxID: "sb", Port: 8080})
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.NotErrorIs(t, err, ErrSandboxLifecycleRejected)
}

// TestLifecycleMatchesPythonWireFixtures pins the ingress lifecycle subset to
// the server's generated Python bindings: identical messages must serialize
// to identical bytes on both sides.
func TestLifecycleMatchesPythonWireFixtures(t *testing.T) {
	target := EndpointTarget{Namespace: "tenant-a", SandboxID: "sandbox-123", Port: 8080}

	t.Run("get_sandbox_paused", func(t *testing.T) {
		fixture := loadWireFixture(t, "get_sandbox_paused.json")
		lifecycle := &scriptedLifecycleRPCs{getSandboxFunc: func(context.Context, *fastpathv2.GetSandboxRequest) (*fastpathv2.GetSandboxResponse, error) {
			response := &fastpathv2.GetSandboxResponse{}
			responseBytes, err := hex.DecodeString(fixture["response_hex"])
			require.NoError(t, err)
			require.NoError(t, proto.Unmarshal(responseBytes, response))
			return response, nil
		}}
		provider, err := NewFastSandboxProviderWithLifecycle(&fakeFastPathResolver{now: time.Now()}, lifecycle, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
		require.NoError(t, err)
		probe, err := provider.ProbeSandbox(context.Background(), target)
		require.NoError(t, err)
		require.Equal(t, SandboxPhaseWakeable, probe.Phase)
		require.Equal(t, "ckpt-1", probe.CheckpointID)

		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(lifecycle.getSandboxRequests[0])
		require.NoError(t, err)
		require.Equal(t, fixture["request_hex"], hex.EncodeToString(encoded))
	})

	t.Run("resume_sandbox", func(t *testing.T) {
		fixture := loadWireFixture(t, "resume_sandbox.json")
		lifecycle := &scriptedLifecycleRPCs{resumeFunc: func(context.Context, *fastpathv2.ResumeSandboxRequest) (*fastpathv2.ResumeSandboxResponse, error) {
			return &fastpathv2.ResumeSandboxResponse{}, nil
		}}
		provider, err := NewFastSandboxProviderWithLifecycle(&fakeFastPathResolver{now: time.Now()}, lifecycle, time.Second, fastpathv2.EndpointAccessMode_CENTRAL_PROXY)
		require.NoError(t, err)
		outcome, err := provider.ResumeSandbox(context.Background(), target, "ckpt-1", "wake-sandbox-123-1")
		require.NoError(t, err)
		require.Equal(t, ResumeAccepted, outcome)

		encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(lifecycle.resumeRequests[0])
		require.NoError(t, err)
		require.Equal(t, fixture["request_hex"], hex.EncodeToString(encoded))
	})
}

func loadWireFixture(t *testing.T, name string) map[string]string {
	t.Helper()
	fixtureBytes, err := os.ReadFile("../fastpath/v2/testdata/" + name)
	require.NoError(t, err)
	var fixture map[string]string
	require.NoError(t, json.Unmarshal(fixtureBytes, &fixture))
	return fixture
}
