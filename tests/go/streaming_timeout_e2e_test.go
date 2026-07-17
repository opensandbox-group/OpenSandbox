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
	"testing"
	"time"

	"github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/stretchr/testify/require"
)

// TestStreaming_NotKilledByRequestTimeout verifies that a command whose SSE
// output stream lasts longer than the connection's RequestTimeout still runs to
// completion. Streaming uses a dedicated client with no overall request timeout,
// so RequestTimeout (which bounds normal request/response) must not kill a
// long-running command mid-stream.
//
// Before the fix, the SSE read shared the request timeout and failed with
// "sse read: context deadline exceeded (Client.Timeout ... while reading body)".
func TestStreaming_NotKilledByRequestTimeout(t *testing.T) {
	// Provision the sandbox with a generous streaming config (reliable startup).
	ctx, sb := createTestSandbox(t)

	// Reconnect with a SHORT RequestTimeout to prove streaming ignores it.
	// Connection setup (GetEndpoint/ping) is fast and stays within this bound.
	shortCfg := getConnectionConfig(t)
	shortCfg.RequestTimeout = 5 * time.Second

	sbShort, err := opensandbox.ConnectSandbox(ctx, shortCfg, sb.ID())
	require.NoError(t, err, "reconnect with short RequestTimeout should succeed")

	// Command runs ~8s > the 5s RequestTimeout.
	start := time.Now()
	exec, err := sbShort.RunCommand(ctx, "echo start-long; sleep 8; echo done-long", nil)
	elapsed := time.Since(start)

	require.NoError(t, err, "streaming command longer than RequestTimeout must not be killed")
	require.Greater(t, elapsed, 5*time.Second, "command should have run past the RequestTimeout")
	require.NotNil(t, exec.ExitCode)
	require.Equal(t, 0, *exec.ExitCode)
	require.Contains(t, exec.Text(), "done-long")
	t.Logf("long streaming command completed in %v (RequestTimeout was 5s)", elapsed.Round(time.Second))
}
