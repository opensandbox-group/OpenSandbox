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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/alibaba/opensandbox/execd/pkg/runtime"
)

func TestRunInSession_CwdWithExecdEnvsVarPassesValidation(t *testing.T) {
	requireBash(t)

	workspace := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(envFile, []byte("SESSION_WORKSPACE="+workspace), 0o644))
	t.Setenv("EXECD_ENVS", envFile)

	previousRunner := codeRunner
	codeRunner = runtime.NewController("", "")
	t.Cleanup(func() { codeRunner = previousRunner })
	runner := codeRunner.(*runtime.Controller)

	sessionID, err := runner.CreateBashSession(&runtime.CreateContextRequest{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.DeleteBashSession(sessionID) })

	body := fmt.Sprintf(`{"command":"pwd","cwd":"$SESSION_WORKSPACE","timeout":0}`)
	ctx, w := newTestContext(http.MethodPost, "/sessions/"+sessionID+"/run", []byte(body))
	ctx.Params = append(ctx.Params, gin.Param{Key: "sessionId", Value: sessionID})
	ctrl := NewCodeInterpretingController(ctx)

	ctrl.RunInSession()

	require.Equal(t, http.StatusOK, w.Code, "cwd referencing an EXECD_ENVS variable must pass validation: %s", w.Body.String())
	require.Contains(t, w.Body.String(), workspace)
}
