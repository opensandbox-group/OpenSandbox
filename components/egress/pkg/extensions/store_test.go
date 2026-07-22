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

package extensions

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type testApplier struct {
	err error
}

func (a testApplier) Apply(_ context.Context, _ uint64, _ []Resource) (ApplyResult, error) {
	if a.err != nil {
		return ApplyResult{}, a.err
	}
	return ApplyResult{ProgrammedStatus: "True", Reason: "Acknowledged"}, nil
}

func httpRouteResource() Resource {
	return Resource{
		APIVersion: "gateway.networking.k8s.io/v1",
		Kind:       "HTTPRoute",
		Metadata: map[string]any{
			"name": "api-example",
			"labels": map[string]any{
				"sandbox.opensandbox.io/sandbox": "sandbox-1",
			},
		},
		Spec: map[string]any{
			"hostnames": []any{"api.example.com"},
			"futureField": map[string]any{
				"preserved": true,
			},
		},
	}
}

func TestStoreReplaceRoundTripsAndChecksRevision(t *testing.T) {
	store := NewStore(testApplier{})
	resource := httpRouteResource()

	state, apiErr := store.Replace(context.Background(), ReplaceRequest{Resources: []Resource{resource}})
	require.Nil(t, apiErr)
	require.Equal(t, uint64(1), state.Revision)
	require.Equal(t, "True", state.Conditions[1].Status)
	require.Equal(t, resource.Metadata["labels"], state.Resources[0].Metadata["labels"])
	require.Equal(t, resource.Spec["futureField"], state.Resources[0].Spec["futureField"])

	stale := uint64(0)
	_, apiErr = store.Replace(context.Background(), ReplaceRequest{
		ExpectedRevision: &stale,
		Resources:        []Resource{},
	})
	require.Equal(t, CodeRevisionConflict, apiErr.Code)
	require.Equal(t, uint64(1), store.Current().Revision)
}

func TestStoreRejectsUnsupportedAndUnavailableResources(t *testing.T) {
	store := NewStore(testApplier{})

	unsupported := httpRouteResource()
	unsupported.Kind = "ImaginaryRoute"
	_, apiErr := store.Replace(context.Background(), ReplaceRequest{Resources: []Resource{unsupported}})
	require.Equal(t, CodeUnsupportedExtension, apiErr.Code)

	unavailable := httpRouteResource()
	unavailable.APIVersion = "gateway.networking.k8s.io/v1alpha2"
	unavailable.Kind = "TLSRoute"
	_, apiErr = store.Replace(context.Background(), ReplaceRequest{Resources: []Resource{unavailable}})
	require.Equal(t, CodeCapabilityUnavailable, apiErr.Code)
	require.Zero(t, store.Current().Revision)
}

func TestStoreDoesNotPublishFailedApply(t *testing.T) {
	store := NewStore(testApplier{err: errors.New("nack")})

	_, apiErr := store.Replace(context.Background(), ReplaceRequest{Resources: []Resource{httpRouteResource()}})
	require.Equal(t, CodeApplyFailed, apiErr.Code)
	require.Zero(t, store.Current().Revision)
	require.Empty(t, store.Current().Resources)
}

func TestStoreClearRunsThroughApplier(t *testing.T) {
	var applied [][]Resource
	applier := applyFunc(func(_ context.Context, _ uint64, resources []Resource) (ApplyResult, error) {
		cloned, err := cloneResources(resources)
		require.NoError(t, err)
		applied = append(applied, cloned)
		return ApplyResult{ProgrammedStatus: "True", Reason: "Acknowledged"}, nil
	})
	store := NewStore(applier)

	state, apiErr := store.Replace(context.Background(), ReplaceRequest{Resources: []Resource{httpRouteResource()}})
	require.Nil(t, apiErr)
	expected := state.Revision
	state, apiErr = store.Replace(context.Background(), ReplaceRequest{
		ExpectedRevision: &expected,
		Resources:        []Resource{},
	})
	require.Nil(t, apiErr)
	require.Equal(t, uint64(2), state.Revision)
	require.Len(t, applied, 2)
	require.Empty(t, applied[1])
}

func TestStoreCapabilitiesFollowApplierAvailability(t *testing.T) {
	disabled := NewStore(nil).Capabilities()
	enabled := NewStore(POCApplier{}).Capabilities()

	require.Equal(t, ProtocolVersion, enabled.ProtocolVersion)
	require.NotEmpty(t, enabled.Resources)
	require.False(t, capabilityFor(t, disabled, "gateway.networking.k8s.io/v1", "HTTPRoute").Available)
	require.True(t, capabilityFor(t, enabled, "gateway.networking.k8s.io/v1", "HTTPRoute").Available)
	require.Equal(t, []string{"read", "replace"}, capabilityFor(t, enabled, "gateway.networking.k8s.io/v1", "HTTPRoute").Operations)
	require.False(t, capabilityFor(t, enabled, "gateway.networking.k8s.io/v1alpha2", "TLSRoute").Available)
}

func capabilityFor(t *testing.T, capabilities CapabilitiesResponse, apiVersion, kind string) Capability {
	t.Helper()
	for _, capability := range capabilities.Resources {
		if capability.APIVersion == apiVersion && capability.Kind == kind {
			return capability
		}
	}
	t.Fatalf("capability %s %s not found", apiVersion, kind)
	return Capability{}
}

type applyFunc func(context.Context, uint64, []Resource) (ApplyResult, error)

func (f applyFunc) Apply(ctx context.Context, revision uint64, resources []Resource) (ApplyResult, error) {
	return f(ctx, revision, resources)
}
