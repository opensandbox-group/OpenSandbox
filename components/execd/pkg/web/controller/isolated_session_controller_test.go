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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/isolation"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
)

func TestClassifyIsolatedCreateError_UidModeUnavailable(t *testing.T) {
	status, code := classifyIsolatedCreateError(
		fmt.Errorf("create session: %w: userns", runtime.ErrUidModeUnavailable),
	)
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if code != model.ErrorCodeNotSupported {
		t.Errorf("code = %q, want %q", code, model.ErrorCodeNotSupported)
	}
}

func TestClassifyIsolatedCreateError_ServiceUnavailable(t *testing.T) {
	for _, target := range []error{
		runtime.ErrSessionNamespaceUnavailable,
		runtime.ErrIsolatedRunnerClosed,
	} {
		status, code := classifyIsolatedCreateError(
			fmt.Errorf("create session: %w", target),
		)
		if status != http.StatusServiceUnavailable {
			t.Errorf(
				"status for %v = %d, want %d",
				target,
				status,
				http.StatusServiceUnavailable,
			)
		}
		if code != model.ErrorCodeServiceUnavailable {
			t.Errorf(
				"code for %v = %q, want %q",
				target,
				code,
				model.ErrorCodeServiceUnavailable,
			)
		}
	}
}

func TestClassifyIsolatedDeleteError_NamespaceCleanup(t *testing.T) {
	status, code := classifyIsolatedDeleteError(
		fmt.Errorf(
			"delete session: %w",
			runtime.ErrSessionNamespaceCleanup,
		),
	)
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if code != model.ErrorCodeServiceUnavailable {
		t.Errorf(
			"code = %q, want %q",
			code,
			model.ErrorCodeServiceUnavailable,
		)
	}
}

func TestCapabilities_ReportsModeSpecificProbeResults(t *testing.T) {
	previousRunner := isolatedRunner
	previousProbe := isolatedProbeResult
	isolatedRunner = nil
	isolatedProbeResult = &isolation.ProbeResult{
		Available:        false,
		Isolator:         "bwrap",
		Version:          "0.11.0",
		Message:          "no uid mode available",
		SetprivAvailable: false,
		UsernsAvailable:  false,
	}
	t.Cleanup(func() {
		isolatedRunner = previousRunner
		isolatedProbeResult = previousProbe
	})

	ctx, recorder := newTestContext(http.MethodGet, "/v1/isolated/capabilities", nil)
	NewIsolatedSessionController(ctx).Capabilities()

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var response model.CapabilitiesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Isolator != "bwrap" || response.Version != "0.11.0" {
		t.Errorf("isolator/version = %q/%q", response.Isolator, response.Version)
	}
	if response.SetprivAvailable || response.UsernsAvailable {
		t.Errorf("uid modes should be unavailable: %+v", response)
	}

	var raw map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	for _, key := range []string{"setpriv_available", "userns_available"} {
		if value, ok := raw[key]; !ok || value != false {
			t.Errorf("%s = %#v, present=%v; want false and present", key, value, ok)
		}
	}
}

func TestUnavailableAdmissionDoesNotBlockSessionCleanupRoutes(t *testing.T) {
	previousRunner := isolatedRunner
	runner, err := runtime.NewIsolatedRunner(
		runtime.NewController("", ""),
		unavailableTestIsolator{},
		isolation.Config{
			UpperRoot:     t.TempDir(),
			UpperMaxBytes: 8 << 30,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	isolatedRunner = runner
	t.Cleanup(func() {
		runner.StopGC()
		isolatedRunner = previousRunner
	})

	createCtx, createRecorder := newTestContext(
		http.MethodPost,
		"/v1/isolated/session",
		nil,
	)
	NewIsolatedSessionController(createCtx).Create()
	if createRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"Create status with unavailable admission = %d, want %d",
			createRecorder.Code,
			http.StatusServiceUnavailable,
		)
	}

	deleteCtx, deleteRecorder := newTestContext(
		http.MethodDelete,
		"/v1/isolated/session/existing",
		nil,
	)
	deleteCtx.Params = gin.Params{{Key: "sessionId", Value: "existing"}}
	NewIsolatedSessionController(deleteCtx).Delete()
	if deleteRecorder.Code != http.StatusNotFound {
		t.Fatalf(
			"Delete status with unavailable admission = %d, want %d",
			deleteRecorder.Code,
			http.StatusNotFound,
		)
	}

	filesCtx, filesRecorder := newTestContext(
		http.MethodGet,
		"/v1/isolated/session/existing/files",
		nil,
	)
	filesCtx.Params = gin.Params{{Key: "sessionId", Value: "existing"}}
	_, _, _ = NewIsolatedSessionController(filesCtx).getMergedView()
	if filesRecorder.Code != http.StatusNotFound {
		t.Fatalf(
			"Files status with unavailable admission = %d, want %d",
			filesRecorder.Code,
			http.StatusNotFound,
		)
	}
}

type unavailableTestIsolator struct{}

func (unavailableTestIsolator) Name() string {
	return "unavailable-test"
}

func (unavailableTestIsolator) Available() bool {
	return false
}

func (unavailableTestIsolator) Capabilities() isolation.Capabilities {
	return isolation.Capabilities{}
}

func (unavailableTestIsolator) Wrap(*exec.Cmd, isolation.WrapOptions) error {
	return fmt.Errorf("unavailable")
}

// lifecycleTestIsolator is a happy-path isolator for tests needing real sessions.
type lifecycleTestIsolator struct{}

func (lifecycleTestIsolator) Name() string { return "lifecycle-test" }
func (lifecycleTestIsolator) Available() bool {
	return true
}
func (lifecycleTestIsolator) Capabilities() isolation.Capabilities {
	return isolation.Capabilities{
		Available:              true,
		SetprivAvailable:       true,
		SetprivSwitchAvailable: true,
		UsernsAvailable:        true,
	}
}
func (lifecycleTestIsolator) Wrap(*exec.Cmd, isolation.WrapOptions) error { return nil }
func (lifecycleTestIsolator) WrapWithLifecycle(
	cmd *exec.Cmd,
	opts isolation.WrapOptions,
) (isolation.WorkloadLifecycle, error) {
	if err := (lifecycleTestIsolator{}).Wrap(cmd, opts); err != nil {
		return nil, err
	}
	return &lifecycleTestLifecycle{done: make(chan struct{})}, nil
}

type lifecycleTestLifecycle struct {
	done chan struct{}
}

func (*lifecycleTestLifecycle) WaitForIdentity(context.Context) (isolation.WorkloadIdentity, error) {
	return isolation.WorkloadIdentity{PID: 2, SandboxPID: 1}, nil
}
func (*lifecycleTestLifecycle) MarkReady() error             { return nil }
func (l *lifecycleTestLifecycle) Abort()                     { close(l.done) }
func (l *lifecycleTestLifecycle) DrainDone() <-chan struct{} { return l.done }
func (*lifecycleTestLifecycle) DrainError() error            { return nil }
func (*lifecycleTestLifecycle) ExitCode() (int, bool)        { return 0, true }
func (l *lifecycleTestLifecycle) Close() error               { return nil }

// Exercises the background run endpoints end to end: start, poll status, read logs.
func TestBackgroundRun_HTTPFlow(t *testing.T) {
	previousRunner := isolatedRunner
	runner, err := runtime.NewIsolatedRunner(
		runtime.NewController("", ""),
		lifecycleTestIsolator{},
		isolation.Config{
			UpperRoot:     t.TempDir(),
			UpperMaxBytes: 8 << 30,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	isolatedRunner = runner
	t.Cleanup(func() {
		runner.StopGC()
		isolatedRunner = previousRunner
	})

	sessionID, err := runner.CreateIsolatedSession(&runtime.IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), "ws"),
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.DeleteIsolatedSession(sessionID) })

	runCtx, runRecorder := newTestContext(
		http.MethodPost,
		"/v1/isolated/session/"+sessionID+"/run",
		[]byte(`{"code":"echo http-background","background":true}`),
	)
	runCtx.Params = gin.Params{{Key: "sessionId", Value: sessionID}}
	NewIsolatedSessionController(runCtx).Run()

	if runRecorder.Code != http.StatusAccepted {
		t.Fatalf("run status = %d, want 202; body: %s", runRecorder.Code, runRecorder.Body.String())
	}
	var startResp model.IsolatedBackgroundRunResponse
	if err := json.Unmarshal(runRecorder.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if startResp.SessionID != sessionID || startResp.RunID == "" {
		t.Fatalf("start response = %+v, want session %s and a run ID", startResp, sessionID)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := runner.GetIsolatedBackgroundRun(sessionID, startResp.RunID)
		if err != nil {
			t.Fatalf("GetIsolatedBackgroundRun: %v", err)
		}
		if !snapshot.Running {
			if snapshot.ExitCode == nil || *snapshot.ExitCode != 0 {
				t.Fatalf("exit code = %v, want 0", snapshot.ExitCode)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	statusCtx, statusRecorder := newTestContext(http.MethodGet, "/v1/isolated/session/"+sessionID+"/runs/"+startResp.RunID, nil)
	statusCtx.Params = gin.Params{
		{Key: "sessionId", Value: sessionID},
		{Key: "runId", Value: startResp.RunID},
	}
	NewIsolatedSessionController(statusCtx).GetRunStatus()

	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200; body: %s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var runStatus model.IsolatedRunStatus
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &runStatus); err != nil {
		t.Fatalf("decode status response: %v", err)
	}
	if runStatus.Running || runStatus.ExitCode == nil || *runStatus.ExitCode != 0 {
		t.Errorf("run status = %+v, want finished with exit code 0", runStatus)
	}
	if runStatus.FinishedAt == nil {
		t.Error("FinishedAt missing for finished run")
	}

	logCtx, logRecorder := newTestContext(
		http.MethodGet,
		"/v1/isolated/session/"+sessionID+"/runs/"+startResp.RunID+"/logs",
		nil,
	)
	logCtx.Params = gin.Params{
		{Key: "sessionId", Value: sessionID},
		{Key: "runId", Value: startResp.RunID},
	}
	NewIsolatedSessionController(logCtx).GetRunLogs()

	if logRecorder.Code != http.StatusOK {
		t.Fatalf("logs code = %d, want 200; body: %s", logRecorder.Code, logRecorder.Body.String())
	}
	if !strings.Contains(logRecorder.Body.String(), "http-background") {
		t.Errorf("log body = %q, want to contain http-background", logRecorder.Body.String())
	}
	cursorRaw := logRecorder.Header().Get("EXECD-ISOLATED-TAIL-CURSOR")
	if cursorRaw == "" || cursorRaw == "0" {
		t.Errorf("tail cursor header = %q, want > 0", cursorRaw)
	}

	cursor, err := strconv.ParseInt(cursorRaw, 10, 64)
	if err != nil {
		t.Fatalf("parse cursor %q: %v", cursorRaw, err)
	}
	incCtx, incRecorder := newTestContext(
		http.MethodGet,
		"/v1/isolated/session/"+sessionID+"/runs/"+startResp.RunID+"/logs?cursor="+cursorRaw,
		nil,
	)
	incCtx.Params = gin.Params{
		{Key: "sessionId", Value: sessionID},
		{Key: "runId", Value: startResp.RunID},
	}
	NewIsolatedSessionController(incCtx).GetRunLogs()
	if incRecorder.Code != http.StatusOK || incRecorder.Body.Len() != 0 {
		t.Errorf("incremental logs = (code %d, body %q), want (200, empty)", incRecorder.Code, incRecorder.Body.String())
	}
	_ = cursor
}

// Unknown sessions and run IDs 404 on the background run endpoints.
func TestBackgroundRun_Endpoints404(t *testing.T) {
	previousRunner := isolatedRunner
	runner, err := runtime.NewIsolatedRunner(
		runtime.NewController("", ""),
		lifecycleTestIsolator{},
		isolation.Config{
			UpperRoot:     t.TempDir(),
			UpperMaxBytes: 8 << 30,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	isolatedRunner = runner
	t.Cleanup(func() {
		runner.StopGC()
		isolatedRunner = previousRunner
	})

	runCtx, runRecorder := newTestContext(
		http.MethodPost,
		"/v1/isolated/session/nope/run",
		[]byte(`{"code":"echo hi","background":true}`),
	)
	runCtx.Params = gin.Params{{Key: "sessionId", Value: "nope"}}
	NewIsolatedSessionController(runCtx).Run()
	if runRecorder.Code != http.StatusNotFound {
		t.Errorf("run unknown session: status = %d, want 404", runRecorder.Code)
	}

	statusCtx, statusRecorder := newTestContext(http.MethodGet, "/v1/isolated/session/nope/runs/nope", nil)
	statusCtx.Params = gin.Params{{Key: "sessionId", Value: "nope"}, {Key: "runId", Value: "nope"}}
	NewIsolatedSessionController(statusCtx).GetRunStatus()
	if statusRecorder.Code != http.StatusNotFound {
		t.Errorf("status unknown run: status = %d, want 404", statusRecorder.Code)
	}

	logsCtx, logsRecorder := newTestContext(http.MethodGet, "/v1/isolated/session/nope/runs/nope/logs", nil)
	logsCtx.Params = gin.Params{{Key: "sessionId", Value: "nope"}, {Key: "runId", Value: "nope"}}
	NewIsolatedSessionController(logsCtx).GetRunLogs()
	if logsRecorder.Code != http.StatusNotFound {
		t.Errorf("logs unknown run: status = %d, want 404", logsRecorder.Code)
	}
}

// TestGetRunLogs_RejectsMalformedCursor verifies non-integer and negative
// cursor values are rejected with 400 instead of silently reading from 0.
func TestGetRunLogs_RejectsMalformedCursor(t *testing.T) {
	previousRunner := isolatedRunner
	runner, err := runtime.NewIsolatedRunner(
		runtime.NewController("", ""),
		lifecycleTestIsolator{},
		isolation.Config{
			UpperRoot:     t.TempDir(),
			UpperMaxBytes: 8 << 30,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	isolatedRunner = runner
	t.Cleanup(func() {
		runner.StopGC()
		isolatedRunner = previousRunner
	})

	sessionID, err := runner.CreateIsolatedSession(&runtime.IsolatedSessionOptions{
		WorkspacePath: filepath.Join(t.TempDir(), "ws"),
		WorkspaceMode: "rw",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.DeleteIsolatedSession(sessionID) })
	runID, _, err := runner.RunInIsolatedSessionBackground(sessionID, "echo hi", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, cursor := range []string{"abc", "-1"} {
		logsCtx, logsRecorder := newTestContext(
			http.MethodGet,
			"/v1/isolated/session/"+sessionID+"/runs/"+runID+"/logs?cursor="+cursor,
			nil,
		)
		logsCtx.Params = gin.Params{{Key: "sessionId", Value: sessionID}, {Key: "runId", Value: runID}}
		NewIsolatedSessionController(logsCtx).GetRunLogs()
		if logsRecorder.Code != http.StatusBadRequest {
			t.Errorf("cursor=%q: status = %d, want 400", cursor, logsRecorder.Code)
		}
	}
}
