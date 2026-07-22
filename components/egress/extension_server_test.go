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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
	"github.com/alibaba/opensandbox/egress/pkg/extensions"
	"github.com/stretchr/testify/require"
)

const extensionRequest = `{
  "resources": [{
    "apiVersion": "gateway.networking.k8s.io/v1",
    "kind": "HTTPRoute",
    "metadata": {
      "name": "api-example",
      "futureMetadata": {"preserved": true}
    },
    "spec": {
      "hostnames": ["api.example.com"],
      "futureField": {"preserved": [1, 2, 3]}
    }
  }]
}`

type extensionApplyFunc func(context.Context, uint64, []extensions.Resource) (extensions.ApplyResult, error)

func (f extensionApplyFunc) Apply(ctx context.Context, revision uint64, resources []extensions.Resource) (extensions.ApplyResult, error) {
	return f(ctx, revision, resources)
}

func TestExtensionHandlersPOCRoundTripAndRevisionConflict(t *testing.T) {
	srv := &policyServer{extensionStore: extensions.NewStore(extensions.POCApplier{})}

	put := httptest.NewRequest(http.MethodPut, "/extensions", strings.NewReader(extensionRequest))
	putRecorder := httptest.NewRecorder()
	srv.handleExtensions(putRecorder, put)
	require.Equal(t, http.StatusOK, putRecorder.Code)

	var replaced extensions.State
	require.NoError(t, json.Unmarshal(putRecorder.Body.Bytes(), &replaced))
	require.Equal(t, uint64(1), replaced.Revision)
	require.Equal(t, "Unknown", replaced.Conditions[1].Status)
	require.Equal(t, map[string]any{"preserved": true}, replaced.Resources[0].Metadata["futureMetadata"])

	get := httptest.NewRequest(http.MethodGet, "/extensions", nil)
	getRecorder := httptest.NewRecorder()
	srv.handleExtensions(getRecorder, get)
	require.Equal(t, http.StatusOK, getRecorder.Code)
	require.JSONEq(t, putRecorder.Body.String(), getRecorder.Body.String())

	stale := httptest.NewRequest(http.MethodPut, "/extensions", strings.NewReader(`{"expectedRevision":0,"resources":[]}`))
	staleRecorder := httptest.NewRecorder()
	srv.handleExtensions(staleRecorder, stale)
	require.Equal(t, http.StatusConflict, staleRecorder.Code)
	require.Contains(t, staleRecorder.Body.String(), `"code":"REVISION_CONFLICT"`)
	require.Equal(t, uint64(1), srv.extensionStore.Current().Revision)
}

func TestExtensionHandlersRejectUnavailableAndUnsupportedKinds(t *testing.T) {
	srv := &policyServer{extensionStore: extensions.NewStore(extensions.POCApplier{})}

	unavailable := strings.ReplaceAll(extensionRequest, `"gateway.networking.k8s.io/v1"`, `"gateway.networking.k8s.io/v1alpha2"`)
	unavailable = strings.ReplaceAll(unavailable, `"HTTPRoute"`, `"TLSRoute"`)
	recorder := httptest.NewRecorder()
	srv.handleExtensions(recorder, httptest.NewRequest(http.MethodPut, "/extensions", strings.NewReader(unavailable)))
	require.Equal(t, http.StatusPreconditionFailed, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"CAPABILITY_UNAVAILABLE"`)

	unsupported := strings.ReplaceAll(extensionRequest, `"HTTPRoute"`, `"ImaginaryRoute"`)
	recorder = httptest.NewRecorder()
	srv.handleExtensions(recorder, httptest.NewRequest(http.MethodPut, "/extensions", strings.NewReader(unsupported)))
	require.Equal(t, http.StatusUnprocessableEntity, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"UNSUPPORTED_EXTENSION"`)
	require.Zero(t, srv.extensionStore.Current().Revision)
}

func TestExtensionHandlersKeepPreviousRevisionOnApplyFailure(t *testing.T) {
	applyCalls := 0
	applier := extensionApplyFunc(func(_ context.Context, _ uint64, _ []extensions.Resource) (extensions.ApplyResult, error) {
		applyCalls++
		if applyCalls == 2 {
			return extensions.ApplyResult{}, errors.New("nack")
		}
		return extensions.ApplyResult{ProgrammedStatus: "True", Reason: "Acknowledged"}, nil
	})
	srv := &policyServer{extensionStore: extensions.NewStore(applier)}

	first := httptest.NewRecorder()
	srv.handleExtensions(first, httptest.NewRequest(http.MethodPut, "/extensions", strings.NewReader(extensionRequest)))
	require.Equal(t, http.StatusOK, first.Code)

	secondBody := strings.Replace(extensionRequest, `"resources"`, `"expectedRevision":1,"resources"`, 1)
	second := httptest.NewRecorder()
	srv.handleExtensions(second, httptest.NewRequest(http.MethodPut, "/extensions", strings.NewReader(secondBody)))
	require.Equal(t, http.StatusInternalServerError, second.Code)
	require.Contains(t, second.Body.String(), `"code":"EXTENSION_APPLY_FAILED"`)
	require.Equal(t, uint64(1), srv.extensionStore.Current().Revision)
}

func TestExtensionCapabilitiesDefaultDisabledAndExplicitPOC(t *testing.T) {
	t.Setenv(constants.EnvExtensionsPOC, "")
	disabled := newExtensionStoreFromEnv().Capabilities()
	require.False(t, extensionCapability(t, disabled, "gateway.networking.k8s.io/v1", "HTTPRoute").Available)

	t.Setenv(constants.EnvExtensionsPOC, "true")
	enabledStore := newExtensionStoreFromEnv()
	enabled := enabledStore.Capabilities()
	capability := extensionCapability(t, enabled, "gateway.networking.k8s.io/v1", "HTTPRoute")
	require.True(t, capability.Available)
	require.Contains(t, capability.Features, "extensions.opensandbox.io/StorageOnlyPOC")

	state, apiErr := enabledStore.Replace(context.Background(), extensions.ReplaceRequest{
		Resources: []extensions.Resource{{
			APIVersion: "gateway.networking.k8s.io/v1",
			Kind:       "Gateway",
			Metadata:   map[string]any{"name": "sandbox-egress"},
			Spec:       map[string]any{},
		}},
	})
	require.Nil(t, apiErr)
	require.Equal(t, "Unknown", state.Conditions[1].Status)
}

func TestExtensionHandlersRequireAuthAndRejectUnknownEnvelopeFields(t *testing.T) {
	srv := &policyServer{
		token:          "secret",
		extensionStore: extensions.NewStore(extensions.POCApplier{}),
	}

	unauthorized := httptest.NewRecorder()
	srv.handleExtensionCapabilities(unauthorized, httptest.NewRequest(http.MethodGet, "/capabilities", nil))
	require.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	req := httptest.NewRequest(http.MethodPut, "/extensions", strings.NewReader(`{"resources":[],"unknown":true}`))
	req.Header.Set(constants.EgressAuthTokenHeader, "secret")
	recorder := httptest.NewRecorder()
	srv.handleExtensions(recorder, req)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"code":"INVALID_EXTENSION"`)
	require.Zero(t, srv.extensionStore.Current().Revision)
}

func TestPolicyServerServesExtensionPOCEndToEnd(t *testing.T) {
	t.Setenv(constants.EnvExtensionsPOC, "true")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	server, err := startPolicyServer(
		&stubProxy{},
		nil,
		"dns",
		addr,
		"",
		nil,
		"",
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, server.Shutdown(ctx))
	})

	capabilitiesResponse, err := http.Get("http://" + addr + "/capabilities")
	require.NoError(t, err)
	defer capabilitiesResponse.Body.Close()
	require.Equal(t, http.StatusOK, capabilitiesResponse.StatusCode)
	var capabilities extensions.CapabilitiesResponse
	require.NoError(t, json.NewDecoder(capabilitiesResponse.Body).Decode(&capabilities))
	require.True(t, extensionCapability(t, capabilities, "gateway.networking.k8s.io/v1", "HTTPRoute").Available)

	req, err := http.NewRequest(http.MethodPut, "http://"+addr+"/extensions", strings.NewReader(extensionRequest))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	replaceResponse, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer replaceResponse.Body.Close()
	require.Equal(t, http.StatusOK, replaceResponse.StatusCode)
	var state extensions.State
	require.NoError(t, json.NewDecoder(replaceResponse.Body).Decode(&state))
	require.Equal(t, uint64(1), state.Revision)
	require.Equal(t, "Unknown", state.Conditions[1].Status)
	require.Equal(t, "PocStored", state.Conditions[1].Reason)
}

func extensionCapability(t *testing.T, capabilities extensions.CapabilitiesResponse, apiVersion, kind string) extensions.Capability {
	t.Helper()
	for _, capability := range capabilities.Resources {
		if capability.APIVersion == apiVersion && capability.Kind == kind {
			return capability
		}
	}
	t.Fatalf("capability %s %s not found", apiVersion, kind)
	return extensions.Capability{}
}
