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

//go:build !windows

package controller

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
	"github.com/alibaba/opensandbox/execd/pkg/web/model"
	"github.com/stretchr/testify/require"
)

type correlationFirstFailureWriter struct {
	gin.ResponseWriter
	failed bool
}

func (w *correlationFirstFailureWriter) Write(payload []byte) (int, error) {
	if !w.failed {
		w.failed = true
		return 0, errors.New("early command transport failure")
	}
	return w.ResponseWriter.Write(payload)
}

func TestSSECommandRuntimeOutputBeforeInit(t *testing.T) {
	logPath := captureSSECorrelationLog(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, recorder := newTestContext(http.MethodPost, "/command", nil)
	ctx.Writer = &correlationFirstFailureWriter{ResponseWriter: ctx.Writer}
	c := NewCodeInterpretingController(ctx)
	pingCtx, cancel := context.WithCancel(context.Background())
	hooks, stop := c.setServerEventsHandler(pingCtx)
	defer func() { cancel(); stop() }()

	outputSeen := make(chan struct{})
	var once sync.Once
	stdout, stderr, onInit := hooks.OnExecuteStdout, hooks.OnExecuteStderr, hooks.OnExecuteInit
	hooks.OnExecuteStdout = func(text string) {
		stdout(text)
		once.Do(func() { close(outputSeen) })
	}
	hooks.OnExecuteStderr = func(text string) {
		stderr(text)
		once.Do(func() { close(outputSeen) })
	}
	var id string
	var earlyOutput bool
	hooks.OnExecuteInit = func(session string) {
		id = session
		// Delay only delivery of init to the adapter, not runtime allocation,
		// launch, or the real output workers. Unix already permits this order.
		select {
		case <-outputSeen:
			earlyOutput = true
		case <-time.After(3 * time.Second):
		}
		onInit(session)
	}
	request := &runtime.ExecuteCodeRequest{
		Language: runtime.Command,
		Argv:     []string{executable, "-test.run=^TestSSELogProcess$"},
		Timeout:  5 * time.Second,
		Envs:     map[string]string{"EXECD_SSE_LOG_HELPER": "1", "EXECD_SSE_LOG_EXIT": "0"},
		Hooks:    hooks,
	}
	request.SetDefaultHooks()
	err = runtime.NewController("", "").Execute(request)
	cancel()
	stop()
	require.NoError(t, err)
	require.True(t, earlyOutput, "real command output must arrive before the init adapter runs")
	require.NotEmpty(t, id)
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	text := string(data)
	failure := strings.Index(text, "early command transport failure")
	initLog := strings.Index(text, "sse: command_id="+id+" OnExecuteInit write")
	require.GreaterOrEqual(t, failure, 0)
	require.Greater(t, initLog, failure)
	foundFailure := false
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "early command transport failure") {
			foundFailure = true
			require.Contains(t, line, "sse: command_id="+id+" OnExecuteStd")
		}
	}
	require.True(t, foundFailure)
	foundInit := false
	for _, event := range correlationEvents(t, recorder) {
		if event.Type == model.StreamEventTypeInit {
			foundInit = true
			require.Equal(t, id, event.Text)
		}
	}
	require.True(t, foundInit)
	require.Contains(t, text, "command: command_id="+id+" mode=foreground exited")
}
