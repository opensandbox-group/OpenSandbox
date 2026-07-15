// Copyright 2025 Alibaba Group Holding Ltd.
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
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/flag"
	"github.com/alibaba/opensandbox/execd/pkg/jupyter/execute"
	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

type fakeCodeRunner struct {
	execute          func(request *runtime.ExecuteCodeRequest) error
	runInBashSession func(_ context.Context, _ *runtime.ExecuteCodeRequest) error
}

func (f *fakeCodeRunner) CreateContext(_ *runtime.CreateContextRequest) (string, error) {
	return "", nil
}

func (f *fakeCodeRunner) Execute(request *runtime.ExecuteCodeRequest) error {
	if f.execute != nil {
		return f.execute(request)
	}
	return nil
}

func (f *fakeCodeRunner) GetContext(_ string) (runtime.CodeContext, error) {
	return runtime.CodeContext{}, nil
}

func (f *fakeCodeRunner) GetCommandStatus(_ string) (*runtime.CommandStatus, error) {
	return nil, nil
}

func (f *fakeCodeRunner) ListContext(_ string) ([]runtime.CodeContext, error) {
	return nil, nil
}

func (f *fakeCodeRunner) DeleteLanguageContext(_ runtime.Language) error {
	return nil
}

func (f *fakeCodeRunner) DeleteContext(_ string) error {
	return nil
}

func (f *fakeCodeRunner) CreateBashSession(_ *runtime.CreateContextRequest) (string, error) {
	return "", nil
}

func (f *fakeCodeRunner) RunInBashSession(ctx context.Context, req *runtime.ExecuteCodeRequest) error {
	if f.runInBashSession != nil {
		return f.runInBashSession(ctx, req)
	}
	return nil
}

func (f *fakeCodeRunner) SeekBackgroundCommandOutput(_ string, _ int64) ([]byte, int64, error) {
	return nil, 0, nil
}

func (f *fakeCodeRunner) DeleteBashSession(_ string) error {
	return nil
}

func (f *fakeCodeRunner) Interrupt(_ string) error {
	return nil
}

func (f *fakeCodeRunner) CreatePTYSession(_ string, _ string, _ string) (runtime.PTYSession, error) {
	return nil, nil
}
func (f *fakeCodeRunner) GetPTYSession(_ string) runtime.PTYSession         { return nil }
func (f *fakeCodeRunner) DeletePTYSession(_ string) error                   { return nil }
func (f *fakeCodeRunner) GetPTYSessionStatus(_ string) (bool, int64, error) { return false, 0, nil }

func TestBuildExecuteCodeRequestDefaultsToCommand(t *testing.T) {
	ctrl := &CodeInterpretingController{}
	req := model.RunCodeRequest{
		Code: "echo 1",
		Context: model.CodeContext{
			ID:                 "session-1",
			CodeContextRequest: model.CodeContextRequest{},
		},
	}

	execReq := ctrl.buildExecuteCodeRequest(req)

	require.Equal(t, runtime.Command, execReq.Language, "expected default language")
	require.Equal(t, "session-1", execReq.Context)
	require.Equal(t, "echo 1", execReq.Code)
}

func TestBuildExecuteCodeRequestRespectsLanguage(t *testing.T) {
	ctrl := &CodeInterpretingController{}
	req := model.RunCodeRequest{
		Code: "print(1)",
		Context: model.CodeContext{
			ID: "session-2",
			CodeContextRequest: model.CodeContextRequest{
				Language: "python",
			},
		},
	}

	execReq := ctrl.buildExecuteCodeRequest(req)

	require.Equal(t, runtime.Language("python"), execReq.Language)
}

func TestGetContext_NotFoundReturns404(t *testing.T) {
	ctx, w := newTestContext(http.MethodGet, "/code/contexts/missing", nil)
	ctx.Params = append(ctx.Params, gin.Param{Key: "contextId", Value: "missing"})
	ctrl := NewCodeInterpretingController(ctx)

	previous := codeRunner
	codeRunner = runtime.NewController("", "")
	t.Cleanup(func() { codeRunner = previous })

	ctrl.GetContext()

	require.Equal(t, http.StatusNotFound, w.Code)

	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeContextNotFound, resp.Code)
	require.Equal(t, "context missing not found", resp.Message)
}

func TestGetContext_MissingIDReturns400(t *testing.T) {
	ctx, w := newTestContext(http.MethodGet, "/code/contexts/", nil)
	ctrl := NewCodeInterpretingController(ctx)

	ctrl.GetContext()

	require.Equal(t, http.StatusBadRequest, w.Code)

	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeMissingQuery, resp.Code)
	require.Equal(t, "missing path parameter 'contextId'", resp.Message)
}

func TestRunCodeReturnsBeforeGracefulShutdownTimeoutAfterImmediateComplete(t *testing.T) {
	previousRunner := codeRunner
	previousTimeout := flag.ApiGracefulShutdownTimeout
	codeRunner = &fakeCodeRunner{
		execute: func(request *runtime.ExecuteCodeRequest) error {
			request.Hooks.OnExecuteComplete(5 * time.Millisecond)
			return nil
		},
	}
	flag.ApiGracefulShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		codeRunner = previousRunner
		flag.ApiGracefulShutdownTimeout = previousTimeout
	})

	body := []byte(`{"code":"print(1)","context":{"id":"ctx-1","language":"python"}}`)
	ctx, w := newTestContext(http.MethodPost, "/code/run", body)
	ctrl := NewCodeInterpretingController(ctx)

	start := time.Now()
	ctrl.RunCode()
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, w.Code)
	require.Less(t, elapsed, flag.ApiGracefulShutdownTimeout/2)
}

func TestRunInSessionReturnsBeforeGracefulShutdownTimeoutAfterImmediateComplete(t *testing.T) {
	previousRunner := codeRunner
	previousTimeout := flag.ApiGracefulShutdownTimeout
	codeRunner = &fakeCodeRunner{
		runInBashSession: func(_ context.Context, request *runtime.ExecuteCodeRequest) error {
			request.Hooks.OnExecuteComplete(5 * time.Millisecond)
			return nil
		},
	}
	flag.ApiGracefulShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		codeRunner = previousRunner
		flag.ApiGracefulShutdownTimeout = previousTimeout
	})

	body := []byte(`{"command":"echo hi","timeout":0}`)
	ctx, w := newTestContext(http.MethodPost, "/sessions/session-1/run", body)
	ctx.Params = append(ctx.Params, gin.Param{Key: "sessionId", Value: "session-1"})
	ctrl := NewCodeInterpretingController(ctx)

	start := time.Now()
	ctrl.RunInSession()
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, w.Code)
	require.Less(t, elapsed, flag.ApiGracefulShutdownTimeout/2)
}

func TestRunCodeReturnsBeforeGracefulShutdownTimeoutWhenRequestContextCanceled(t *testing.T) {
	previousRunner := codeRunner
	previousTimeout := flag.ApiGracefulShutdownTimeout
	reqCtx, cancelReq := context.WithCancel(context.Background())
	codeRunner = &fakeCodeRunner{
		execute: func(_ *runtime.ExecuteCodeRequest) error {
			cancelReq()
			return nil
		},
	}
	flag.ApiGracefulShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		codeRunner = previousRunner
		flag.ApiGracefulShutdownTimeout = previousTimeout
		cancelReq()
	})

	body := []byte(`{"code":"print(1)","context":{"id":"ctx-1","language":"python"}}`)
	ctx, w := newTestContext(http.MethodPost, "/code/run", body)
	ctx.Request = ctx.Request.WithContext(reqCtx)
	ctrl := NewCodeInterpretingController(ctx)

	start := time.Now()
	ctrl.RunCode()
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, w.Code)
	require.Less(t, elapsed, flag.ApiGracefulShutdownTimeout/2)
}

func TestRunInSessionReturnsBeforeGracefulShutdownTimeoutWhenRequestContextCanceled(t *testing.T) {
	previousRunner := codeRunner
	previousTimeout := flag.ApiGracefulShutdownTimeout
	reqCtx, cancelReq := context.WithCancel(context.Background())
	codeRunner = &fakeCodeRunner{
		runInBashSession: func(_ context.Context, _ *runtime.ExecuteCodeRequest) error {
			cancelReq()
			return nil
		},
	}
	flag.ApiGracefulShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		codeRunner = previousRunner
		flag.ApiGracefulShutdownTimeout = previousTimeout
		cancelReq()
	})

	body := []byte(`{"command":"echo hi","timeout":0}`)
	ctx, w := newTestContext(http.MethodPost, "/sessions/session-1/run", body)
	ctx.Request = ctx.Request.WithContext(reqCtx)
	ctx.Params = append(ctx.Params, gin.Param{Key: "sessionId", Value: "session-1"})
	ctrl := NewCodeInterpretingController(ctx)

	start := time.Now()
	ctrl.RunInSession()
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, w.Code)
	require.Less(t, elapsed, flag.ApiGracefulShutdownTimeout/2)
}

func TestRunCodeReturnsBeforeGracefulShutdownTimeoutAfterImmediateError(t *testing.T) {
	previousRunner := codeRunner
	previousTimeout := flag.ApiGracefulShutdownTimeout
	codeRunner = &fakeCodeRunner{
		execute: func(request *runtime.ExecuteCodeRequest) error {
			request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "ExecError", EValue: "boom"})
			return nil
		},
	}
	flag.ApiGracefulShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		codeRunner = previousRunner
		flag.ApiGracefulShutdownTimeout = previousTimeout
	})

	body := []byte(`{"code":"print(1)","context":{"id":"ctx-1","language":"python"}}`)
	ctx, w := newTestContext(http.MethodPost, "/code/run", body)
	ctrl := NewCodeInterpretingController(ctx)

	start := time.Now()
	ctrl.RunCode()
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, w.Code)
	require.Less(t, elapsed, flag.ApiGracefulShutdownTimeout/2)
}

func TestRunInSessionReturnsBeforeGracefulShutdownTimeoutAfterImmediateError(t *testing.T) {
	previousRunner := codeRunner
	previousTimeout := flag.ApiGracefulShutdownTimeout
	codeRunner = &fakeCodeRunner{
		runInBashSession: func(_ context.Context, request *runtime.ExecuteCodeRequest) error {
			request.Hooks.OnExecuteError(&execute.ErrorOutput{EName: "ExecError", EValue: "boom"})
			return nil
		},
	}
	flag.ApiGracefulShutdownTimeout = 200 * time.Millisecond
	t.Cleanup(func() {
		codeRunner = previousRunner
		flag.ApiGracefulShutdownTimeout = previousTimeout
	})

	body := []byte(`{"command":"echo hi","timeout":0}`)
	ctx, w := newTestContext(http.MethodPost, "/sessions/session-1/run", body)
	ctx.Params = append(ctx.Params, gin.Param{Key: "sessionId", Value: "session-1"})
	ctrl := NewCodeInterpretingController(ctx)

	start := time.Now()
	ctrl.RunInSession()
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, w.Code)
	require.Less(t, elapsed, flag.ApiGracefulShutdownTimeout/2)
}

// TestRunCodeSyncErrorEmitsJSONNotSSE guards against regression of the bug
// where Execute returning a synchronous error after setupSSEResponse caused
// the client to receive a text/event-stream response with a JSON body, which
// SDKs parsed as zero events ("empty sse stream"). Headers must stay
// uncommitted until the first event so RespondError can produce a proper
// application/json error response.
func TestRunCodeSyncErrorEmitsJSONNotSSE(t *testing.T) {
	previousRunner := codeRunner
	codeRunner = &fakeCodeRunner{
		execute: func(_ *runtime.ExecuteCodeRequest) error {
			return errors.New("synchronous runtime failure")
		},
	}
	t.Cleanup(func() { codeRunner = previousRunner })

	body := []byte(`{"code":"print(1)","context":{"id":"ctx-1","language":"python"}}`)
	ctx, w := newTestContext(http.MethodPost, "/code/run", body)
	ctrl := NewCodeInterpretingController(ctx)

	ctrl.RunCode()

	require.Equal(t, http.StatusInternalServerError, w.Code)
	contentType := w.Header().Get("Content-Type")
	require.Contains(t, contentType, "application/json", "should not commit text/event-stream when no event fires")

	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeRuntimeError, resp.Code)
	require.Contains(t, resp.Message, "synchronous runtime failure")
}

// TestRunInSessionSyncErrorEmitsJSONNotSSE — see TestRunCodeSyncErrorEmitsJSONNotSSE.
func TestRunInSessionSyncErrorEmitsJSONNotSSE(t *testing.T) {
	previousRunner := codeRunner
	codeRunner = &fakeCodeRunner{
		runInBashSession: func(_ context.Context, _ *runtime.ExecuteCodeRequest) error {
			return errors.New("synchronous session failure")
		},
	}
	t.Cleanup(func() { codeRunner = previousRunner })

	body := []byte(`{"command":"echo hi","timeout":0}`)
	ctx, w := newTestContext(http.MethodPost, "/sessions/session-1/run", body)
	ctx.Params = append(ctx.Params, gin.Param{Key: "sessionId", Value: "session-1"})
	ctrl := NewCodeInterpretingController(ctx)

	ctrl.RunInSession()

	require.Equal(t, http.StatusInternalServerError, w.Code)
	contentType := w.Header().Get("Content-Type")
	require.Contains(t, contentType, "application/json", "should not commit text/event-stream when no event fires")

	var resp model.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, model.ErrorCodeRuntimeError, resp.Code)
	require.Contains(t, resp.Message, "synchronous session failure")
}

// TestRunCodeSuccessStillEmitsSSE confirms the lazy header path still produces
// a text/event-stream response when at least one event fires.
func TestRunCodeSuccessStillEmitsSSE(t *testing.T) {
	previousRunner := codeRunner
	previousTimeout := flag.ApiGracefulShutdownTimeout
	codeRunner = &fakeCodeRunner{
		execute: func(request *runtime.ExecuteCodeRequest) error {
			request.Hooks.OnExecuteInit("session-1")
			request.Hooks.OnExecuteComplete(time.Millisecond)
			return nil
		},
	}
	flag.ApiGracefulShutdownTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		codeRunner = previousRunner
		flag.ApiGracefulShutdownTimeout = previousTimeout
	})

	body := []byte(`{"code":"print(1)","context":{"id":"ctx-1","language":"python"}}`)
	ctx, w := newTestContext(http.MethodPost, "/code/run", body)
	ctrl := NewCodeInterpretingController(ctx)

	ctrl.RunCode()

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "text/event-stream")
	require.NotEmpty(t, w.Body.Bytes(), "successful run should write SSE events")
}
