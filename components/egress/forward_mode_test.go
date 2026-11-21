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

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/egress/pkg/constants"
)

func setForwardEnv(t *testing.T) {
	t.Helper()
	t.Setenv(constants.EnvEgressMode, constants.PolicyForward)
	t.Setenv(constants.EnvUpstreamProxy, "https://gateway.example.com:3000")
	t.Setenv(constants.EnvUpstreamProxyAuth, "Bearer token")
	t.Setenv(constants.EnvSandboxID, "sbx-1")
}

func TestForwardModeFromEnvAccepts(t *testing.T) {
	setForwardEnv(t)
	fm, err := forwardModeFromEnv("")
	require.NoError(t, err)
	require.Equal(t, "gateway.example.com", fm.gateway.Host)
	require.Equal(t, 3000, fm.gateway.Port)
	require.False(t, fm.refusedFinal)

	t.Setenv(constants.EnvDNSUpstream, "10.96.0.53")
	fm, err = forwardModeFromEnv("")
	require.NoError(t, err)
	require.True(t, fm.refusedFinal, "configured policy resolver makes REFUSED final")
}

func TestForwardModeFromEnvFailsClosed(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"fast-sandbox profile": func(t *testing.T) {},
		"missing upstream":     func(t *testing.T) { t.Setenv(constants.EnvUpstreamProxy, "") },
		"missing auth":         func(t *testing.T) { t.Setenv(constants.EnvUpstreamProxyAuth, "") },
		"missing sandbox id":   func(t *testing.T) { t.Setenv(constants.EnvSandboxID, "") },
		"bad tenant header":    func(t *testing.T) { t.Setenv(constants.EnvTenantName, "acme corp") },
		"transparent mitm":     func(t *testing.T) { t.Setenv(constants.EnvMitmproxyTransparent, "true") },
		"ssl insecure":         func(t *testing.T) { t.Setenv(constants.EnvMitmproxySslInsecure, "true") },
		"revision runtime":     func(t *testing.T) { t.Setenv(constants.EnvExperimentalRevisionRuntime, "1") },
		"passthrough hosts":    func(t *testing.T) { t.Setenv(constants.EnvMitmproxyPassthroughHosts, "") },
		"nameserver exempt":    func(t *testing.T) { t.Setenv(constants.EnvNameserverExempt, "10.0.0.2") },
		"local policy rules":   func(t *testing.T) { t.Setenv(constants.EnvEgressRules, `{"defaultAction":"allow"}`) },
		"unreadable extra CA": func(t *testing.T) {
			t.Setenv(constants.EnvMitmproxyUpstreamExtraCA, filepath.Join(t.TempDir(), "missing.pem"))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			setForwardEnv(t)
			mutate(t)
			profile := ""
			if name == "fast-sandbox profile" {
				profile = constants.ProfileFastSandbox
			}
			_, err := forwardModeFromEnv(profile)
			require.Error(t, err)
		})
	}
}

func TestGatewayRootCAsRejectsNonPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a certificate"), 0o600))
	_, err := gatewayRootCAs(path)
	require.Error(t, err)

	pool, err := gatewayRootCAs("")
	require.NoError(t, err)
	require.Nil(t, pool, "empty path keeps system roots")
}

func TestForwardHTTPHandler(t *testing.T) {
	var gatewayErr error
	h := forwardHTTPHandler(func() error { return gatewayErr })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"status":"ok","egressMode":"forward","gateway":"reachable"}`, rec.Body.String())

	gatewayErr = errors.New("dial tcp: connection refused")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.JSONEq(t, `{"status":"unavailable","egressMode":"forward","gateway":"unreachable"}`, rec.Body.String())

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/policy", nil))
		require.Equal(t, http.StatusConflict, rec.Code, method)
	}
}

func TestGatewayHealthStartsUnhealthyAndTracksProbe(t *testing.T) {
	h := &gatewayHealth{}
	require.ErrorIs(t, h.err(), errGatewayNotProbed, "unprobed gateway must not report healthy")

	results := make(chan error, 2)
	results <- errors.New("refused")
	results <- nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probed := make(chan struct{}, 2)
	go h.run(ctx, func(context.Context) error {
		select {
		case err := <-results:
			probed <- struct{}{}
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	<-probed
	require.Eventually(t, func() bool { return h.err() != nil && h.err() != errGatewayNotProbed }, time.Second, 10*time.Millisecond)
	<-probed // second probe after the unhealthy period
	require.Eventually(t, func() bool { return h.err() == nil }, 3*time.Second, 10*time.Millisecond)
}
