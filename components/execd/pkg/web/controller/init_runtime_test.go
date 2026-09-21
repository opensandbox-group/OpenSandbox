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

package controller

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/binding"
	"github.com/alibaba/opensandbox/execd/pkg/lifecycle"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

type recordingResetter struct{ resets atomic.Int32 }

func (r *recordingResetter) Reset() error {
	r.resets.Add(1)
	return nil
}

func newTestInitManager(t *testing.T, cfg RuntimeInitConfig) *RuntimeInitManager {
	t.Helper()
	return InitRuntimeInitManager(&cfg)
}

func clearInitManager(t *testing.T) {
	t.Helper()
	runtimeInitManager.Store(nil)
}

func validInitRequest() *model.RuntimeInitRequest {
	return &model.RuntimeInitRequest{
		SandboxID:  "sandbox-123",
		Generation: 7,
		Envs:       map[string]string{"APP_ENV": "production"},
	}
}

func TestApplyDefaultKeepsEntrypoint(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	var launchCalls int
	closer := &recordingResetter{}
	manager := newTestInitManager(t, RuntimeInitConfig{
		IsolatedResetter: closer,
		LaunchEntrypoint: func([]string) error {
			launchCalls++
			return nil
		},
		EntrypointArgs: []string{"/bin/entrypoint"},
	})

	// Default policy is keep: the template entrypoint is NOT started
	// (API-only sandbox), and a warning reports the suppression.
	warnings, _, status, err := manager.Apply(validInitRequest())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Zero(t, launchCalls)
	require.Equal(t, int32(1), closer.resets.Load(), "previous generation sessions torn down")
	require.Contains(t, warnings, "entrypointPolicy=keep: template entrypoint was not started")
	require.True(t, manager.ready.Load(), "ready even without an entrypoint")
}

func TestApplyRestartPolicyLaunchesEntrypoint(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	var launchCalls [][]string
	manager := newTestInitManager(t, RuntimeInitConfig{
		LaunchEntrypoint: func(args []string) error {
			cp := append([]string(nil), args...)
			launchCalls = append(launchCalls, cp)
			return nil
		},
		EntrypointArgs: []string{"/bin/entrypoint"},
	})

	req := validInitRequest()
	req.EntrypointPolicy = model.EntrypointPolicyRestart
	warnings, _, status, err := manager.Apply(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, warnings)
	require.Equal(t, [][]string{{"/bin/entrypoint"}}, launchCalls)

	current := binding.Current()
	require.NotNil(t, current)
	require.Equal(t, "sandbox-123", current.SandboxID)
	require.EqualValues(t, 7, current.Generation)
	require.Equal(t, "production", current.Envs["APP_ENV"])
	require.True(t, manager.ready.Load(), "manager must be ready after successful apply")
}

func TestApplyRestartPolicyIgnoredInClassicMode(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{
		// No LaunchEntrypoint: classic mode, execd does not own the
		// entrypoint.
		EntrypointArgs: []string{"/bin/entrypoint"},
	})

	req := validInitRequest()
	req.EntrypointPolicy = model.EntrypointPolicyRestart
	warnings, _, status, err := manager.Apply(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, warnings, "entrypointPolicy=restart ignored: execd does not own the entrypoint in this mode")
}

func TestApplyEntrypointPolicyValidation(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{})

	req := validInitRequest()
	req.EntrypointPolicy = "bogus"
	_, _, status, err := manager.Apply(req)
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, status)
	require.False(t, manager.accepted.Load(), "invalid policy must not consume the slot")
}

func TestApplyStrictlyOnce(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{})

	_, _, status, err := manager.Apply(validInitRequest())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	// Any second call is rejected — identical retry or different sandbox.
	for name, req := range map[string]*model.RuntimeInitRequest{
		"identical retry":   {SandboxID: "sandbox-123", Generation: 7},
		"different sandbox": {SandboxID: "sandbox-other", Generation: 8},
		"generation replay": {SandboxID: "sandbox-123", Generation: 6},
		"higher generation": {SandboxID: "sandbox-123", Generation: 99},
	} {
		t.Run(name, func(t *testing.T) {
			_, code, status, err := manager.Apply(req)
			require.ErrorIs(t, err, ErrAlreadyInitialized)
			require.Equal(t, model.ErrorCodeAlreadyInitialized, code)
			require.Equal(t, http.StatusConflict, status)
		})
	}

	// The applied binding was never replaced.
	require.Equal(t, "sandbox-123", binding.Current().SandboxID)
	require.EqualValues(t, 7, binding.Current().Generation)
}

func TestApplyInvalidRequestDoesNotConsumeSlot(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{})

	// A malformed call is rejected without consuming the one-shot slot.
	_, _, status, err := manager.Apply(&model.RuntimeInitRequest{Generation: 1})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, status)
	require.False(t, manager.accepted.Load())
	require.Nil(t, binding.Current())

	// The next valid call initializes normally.
	_, _, status, err = manager.Apply(validInitRequest())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.True(t, manager.ready.Load())
}

func TestApplyFailedApplyConsumesSlot(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{
		LaunchEntrypoint: func([]string) error { return errFakeEntrypoint },
		EntrypointArgs:   []string{"/bin/entrypoint"},
	})

	req := validInitRequest()
	req.EntrypointPolicy = model.EntrypointPolicyRestart

	// The apply fails (500) but consumes the slot...
	_, _, status, err := manager.Apply(req)
	require.ErrorIs(t, err, errFakeEntrypoint)
	require.Equal(t, http.StatusInternalServerError, status)
	require.False(t, manager.ready.Load())

	// ...so a retry is rejected: the control plane recycles the container.
	_, code, status, err := manager.Apply(validInitRequest())
	require.ErrorIs(t, err, ErrAlreadyInitialized)
	require.Equal(t, model.ErrorCodeAlreadyInitialized, code)
	require.Equal(t, http.StatusConflict, status)
}

func TestApplyValidationFailures(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{})

	tests := []struct {
		name    string
		request *model.RuntimeInitRequest
	}{
		{"blank sandbox id", &model.RuntimeInitRequest{Generation: 1}},
		{"zero generation", &model.RuntimeInitRequest{SandboxID: "s", Generation: 0}},
		{"reserved env key", &model.RuntimeInitRequest{
			SandboxID:  "s",
			Generation: 1,
			Envs:       map[string]string{"EXECD_ACCESS_TOKEN": "smuggled"},
		}},
		{"bad token hash", &model.RuntimeInitRequest{
			SandboxID:       "s",
			Generation:      1,
			AccessTokenHash: "not-a-hash",
		}},
		{"invalid lifecycle", &model.RuntimeInitRequest{
			SandboxID:  "s",
			Generation: 1,
			Lifecycle:  &lifecycle.Config{Version: 99},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, status, err := manager.Apply(tt.request)
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, status)
			require.Nil(t, binding.Current(), "failed validation must not apply a binding")
			require.False(t, manager.ready.Load())
		})
	}
}

func TestApplyWithTokenHash(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{})
	req := validInitRequest()
	req.AccessTokenHash = binding.HashAccessToken("rotated-token")

	_, _, status, err := manager.Apply(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	current := binding.Current()
	require.True(t, current.HasAccessToken)
	require.True(t, current.VerifyAccessToken("rotated-token"))
	require.False(t, current.VerifyAccessToken("legacy-token"))
}

func TestApplyWithTelemetryAttrs(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{})
	req := validInitRequest()
	req.Telemetry = &model.RuntimeInitTelemetry{
		Attributes: map[string]string{"tenant_id": "tenant-a", "sandbox_id": "spoof"},
	}

	warnings, _, status, err := manager.Apply(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, warnings, 1, "reserved attribute dropped with a warning")

	current := binding.Current()
	require.Equal(t, "tenant-a", current.TelemetryAttrs["tenant_id"])
	require.NotContains(t, current.TelemetryAttrs, "sandbox_id")
}

func TestApplyLifecycleOverrideAndTemplateFallback(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	template := &lifecycle.Config{Version: 1, Periodic: []lifecycle.PeriodicHook{{
		Name: "template-hook", Schedule: "@every 60s", Command: []string{"/bin/true"},
	}}}
	var statuses []string
	statusRecorder := func(status string) error {
		statuses = append(statuses, status)
		return nil
	}

	// Omitted lifecycle keeps the template periodic hooks running.
	manager := newTestInitManager(t, RuntimeInitConfig{
		TemplateLifecycle:   template,
		AppendStartupStatus: statusRecorder,
	})
	_, _, status, err := manager.Apply(validInitRequest())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.NotNil(t, manager.periodic, "template periodic hooks must keep running")
	manager.StopPeriodic()

	// A fresh execd (new manager) with an explicit (empty) lifecycle
	// replaces the template-level one.
	manager2 := newTestInitManager(t, RuntimeInitConfig{
		TemplateLifecycle:   template,
		AppendStartupStatus: statusRecorder,
	})
	_, _, status, err = manager2.Apply(&model.RuntimeInitRequest{
		SandboxID:  "sandbox-123",
		Generation: 8,
		Lifecycle:  &lifecycle.Config{},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	require.Nil(t, manager2.periodic, "explicit empty lifecycle stops periodic hooks")
}

func TestReadyHandlerStates(t *testing.T) {
	clearInitManager(t)
	prev := binding.Apply(nil)
	t.Cleanup(func() { binding.Apply(prev) })

	manager := newTestInitManager(t, RuntimeInitConfig{})
	binding.Apply(&binding.RuntimeBinding{SandboxID: "sandbox-1", Generation: 3})

	// Uninitialized: 503.
	ctx, w := newTestContext(http.MethodGet, "/ready", nil)
	NewInitController(ctx).Ready()
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	// After apply: 200 with binding echo.
	manager.MarkReady()
	ctx, w = newTestContext(http.MethodGet, "/ready", nil)
	NewInitController(ctx).Ready()
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"initialized":true`)
	require.Contains(t, w.Body.String(), `"sandboxId":"sandbox-1"`)
}

type staticError struct{}

func (staticError) Error() string { return "static error" }

var errFakeEntrypoint = staticError{}
