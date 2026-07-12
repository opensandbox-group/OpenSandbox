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

package e2e

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func TestEgressEnv_InjectedIntoSidecar(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env: map[string]string{
			"EXECD_API_GRACE_SHUTDOWN":        "3s",
			"EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms",
			"OPENSANDBOX_EGRESS_LOG_LEVEL":    "debug",
			"MY_APP_VAR":                      "hello",
		},
		NetworkPolicy: &opensandbox.NetworkPolicy{
			DefaultAction: "allow",
		},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	require.True(t, sb.IsHealthy(ctx), "sandbox should be healthy")

	// OPENSANDBOX_EGRESS_LOG_LEVEL should NOT be in sandbox container env
	exec, err := sb.RunCommand(ctx, "printenv OPENSANDBOX_EGRESS_LOG_LEVEL || echo __unset__", nil)
	require.NoError(t, err)
	output := strings.TrimSpace(exec.Text())
	require.Equal(t, "__unset__", output, "OPENSANDBOX_EGRESS_ var should not leak into sandbox container")

	// MY_APP_VAR should be in sandbox container env
	exec, err = sb.RunCommand(ctx, "printenv MY_APP_VAR", nil)
	require.NoError(t, err)
	require.Contains(t, strings.TrimSpace(exec.Text()), "hello")

	t.Log("Egress env correctly split: OPENSANDBOX_EGRESS_* routed to sidecar, regular env stays in sandbox")
}

func TestEgressEnv_ReservedVarReturns400(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := opensandbox.NewLifecycleClient(config.Protocol+"://"+config.Domain+"/v1", config.APIKey)

	_, err := client.CreateSandbox(ctx, opensandbox.CreateSandboxRequest{
		Image:      &opensandbox.ImageSpec{URI: getSandboxImage()},
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		ResourceLimits: opensandbox.ResourceLimits{
			"cpu":    "500m",
			"memory": "256Mi",
		},
		Env: map[string]string{
			"OPENSANDBOX_EGRESS_RULES": "should-be-rejected",
		},
		NetworkPolicy: &opensandbox.NetworkPolicy{
			DefaultAction: "allow",
		},
	})
	require.Error(t, err)

	var apiErr *opensandbox.APIError
	require.True(t, errors.As(err, &apiErr), "expected APIError, got %T: %v", err, err)
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode,
		"disallowed OPENSANDBOX_EGRESS_ var should return 400, got %d", apiErr.StatusCode)

	t.Logf("Disallowed env var correctly rejected: %s", apiErr.Error())
}

func TestEgressEnv_SSLInsecureWithCredentialProxyReturns400(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := opensandbox.NewLifecycleClient(config.Protocol+"://"+config.Domain+"/v1", config.APIKey)

	_, err := client.CreateSandbox(ctx, opensandbox.CreateSandboxRequest{
		Image:      &opensandbox.ImageSpec{URI: getSandboxImage()},
		Entrypoint: []string{"tail", "-f", "/dev/null"},
		ResourceLimits: opensandbox.ResourceLimits{
			"cpu":    "500m",
			"memory": "256Mi",
		},
		Env: map[string]string{
			"OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE": "true",
		},
		NetworkPolicy: &opensandbox.NetworkPolicy{
			DefaultAction: "allow",
		},
		CredentialProxy: &opensandbox.CredentialProxyConfig{Enabled: true},
	})
	require.Error(t, err)

	var apiErr *opensandbox.APIError
	require.True(t, errors.As(err, &apiErr), "expected APIError, got %T: %v", err, err)
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode,
		"SSL_INSECURE + credential proxy should return 400, got %d", apiErr.StatusCode)

	t.Logf("SSL_INSECURE + credential proxy correctly rejected: %s", apiErr.Error())
}

func TestEgressEnv_NoNetworkPolicyDoesNotBlock(t *testing.T) {
	config := getConnectionConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Egress env vars without networkPolicy should not block sandbox creation
	// (server logs a warning, vars are silently dropped)
	sb, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(),
		Env: map[string]string{
			"EXECD_API_GRACE_SHUTDOWN":        "3s",
			"EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms",
			"OPENSANDBOX_EGRESS_LOG_LEVEL":    "debug",
		},
	})
	require.NoError(t, err)
	defer sb.Kill(context.Background())

	require.True(t, sb.IsHealthy(ctx), "sandbox should be healthy even with egress env and no networkPolicy")

	// The egress var should not be in the sandbox env
	exec, err := sb.RunCommand(ctx, "printenv OPENSANDBOX_EGRESS_LOG_LEVEL || echo __unset__", nil)
	require.NoError(t, err)
	output := strings.TrimSpace(exec.Text())
	require.Equal(t, "__unset__", output,
		"OPENSANDBOX_EGRESS_ var should be dropped when no networkPolicy is set")

	t.Log("Sandbox created successfully with egress env but no networkPolicy — vars dropped as expected")
}
