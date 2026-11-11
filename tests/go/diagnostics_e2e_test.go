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

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

func TestDiagnosticsE2E(t *testing.T) {
	config := connectionConfigForStreaming(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	manager := opensandbox.NewSandboxManager(config)
	defer manager.Close()
	const marker = "opensandbox-diagnostics-e2e"
	ttlSeconds := 300
	sandbox, err := opensandbox.CreateSandbox(ctx, config, opensandbox.SandboxCreateOptions{
		Image: getSandboxImage(), Entrypoint: []string{"sh", "-c", "echo " + marker + "; sleep 300"},
		TimeoutSeconds: &ttlSeconds, ResourceLimits: getE2eSandboxResource(),
		Env: map[string]string{"EXECD_API_GRACE_SHUTDOWN": "1s"},
	})
	require.NoError(t, err)
	defer func() {
		defer sandbox.Close()
		cleanup, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		require.NoError(t, sandbox.Kill(cleanup))
	}()

	t.Run("context", func(t *testing.T) {
		result, err := manager.GetDiagnosticContext(ctx, sandbox.ID())
		require.NoError(t, err)
		current, err := sandbox.GetDiagnosticContext(ctx)
		require.NoError(t, err)
		require.Equal(t, sandbox.ID(), result.SandboxID)
		require.Equal(t, result.BackendType, current.BackendType)
		require.Equal(t, result.BackendIdentifiers, current.BackendIdentifiers)
		require.NotNil(t, result.State)
		if result.BackendType == "docker" {
			require.NotEmpty(t, result.BackendIdentifiers["containerId"])
			require.NotEmpty(t, result.BackendIdentifiers["containerName"])
		} else {
			require.Equal(t, "kubernetes", result.BackendType)
			require.NotEmpty(t, result.BackendIdentifiers["namespace"])
			require.NotEmpty(t, result.BackendIdentifiers["workloadName"])
		}
		t.Logf("diagnostic context: %+v", result)
	})
	for _, scope := range []string{"container", "all"} {
		t.Run("logs/"+scope, func(t *testing.T) {
			for _, getLogs := range []func() (*opensandbox.DiagnosticContent, error){
				func() (*opensandbox.DiagnosticContent, error) {
					return manager.GetDiagnosticLogs(ctx, sandbox.ID(), scope)
				},
				func() (*opensandbox.DiagnosticContent, error) { return sandbox.GetDiagnosticLogs(ctx, scope) },
			} {
				deadline := time.Now().Add(30 * time.Second)
				for {
					result, err := getLogs()
					require.NoError(t, err)
					assertDiagnosticContent(t, result, sandbox.ID(), "logs", scope)
					if result.Content != nil && strings.Contains(*result.Content, marker) {
						break
					}
					require.True(t, time.Now().Before(deadline), "entrypoint log missing: %+v", result)
					time.Sleep(500 * time.Millisecond)
				}
			}
		})
	}
	for _, scope := range []string{"runtime", "all"} {
		t.Run("events/"+scope, func(t *testing.T) {
			result, err := manager.GetDiagnosticEvents(ctx, sandbox.ID(), scope)
			require.NoError(t, err)
			assertDiagnosticContent(t, result, sandbox.ID(), "events", scope)
			current, err := sandbox.GetDiagnosticEvents(ctx, scope)
			require.NoError(t, err)
			assertDiagnosticContent(t, current, sandbox.ID(), "events", scope)
		})
	}
	t.Run("unsupported scope", func(t *testing.T) {
		_, logErr := manager.GetDiagnosticLogs(ctx, sandbox.ID(), "unsupported-e2e-scope")
		_, eventErr := manager.GetDiagnosticEvents(ctx, sandbox.ID(), "unsupported-e2e-scope")
		for _, err := range []error{logErr, eventErr} {
			var apiError *opensandbox.APIError
			require.True(t, errors.As(err, &apiError), "%v", err)
			require.Equal(t, 400, apiError.StatusCode)
			require.Equal(t, "DIAGNOSTICS_SCOPE_UNSUPPORTED", apiError.Response.Code)
		}
	})
	t.Run("missing sandbox", func(t *testing.T) {
		id := fmt.Sprintf("missing-%d", time.Now().UnixNano())
		_, contextErr := manager.GetDiagnosticContext(ctx, id)
		_, logErr := manager.GetDiagnosticLogs(ctx, id, "container")
		_, eventErr := manager.GetDiagnosticEvents(ctx, id, "runtime")
		for _, err := range []error{contextErr, logErr, eventErr} {
			var apiError *opensandbox.APIError
			require.True(t, errors.As(err, &apiError), "%v", err)
			require.Equal(t, 404, apiError.StatusCode)
		}
	})
}

func assertDiagnosticContent(t *testing.T, result *opensandbox.DiagnosticContent, id, kind, scope string) {
	t.Helper()
	require.Equal(t, id, result.SandboxID)
	require.Equal(t, kind, result.Kind)
	require.Equal(t, scope, result.Scope)
	require.Equal(t, "inline", result.Delivery)
	require.Contains(t, result.ContentType, "text/plain")
	require.NotNil(t, result.Content)
}
