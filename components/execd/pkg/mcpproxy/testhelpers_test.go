// Copyright 2025 The OpenSandbox Authors
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

package mcpproxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// testHTTPServer wraps an httptest server around a bridge handler.
type testHTTPServer struct {
	server  *httptest.Server
	baseURL string
}

func newTestHTTPServer(h http.Handler) *testHTTPServer {
	server := httptest.NewServer(h)
	return &testHTTPServer{server: server, baseURL: server.URL}
}

// snapshotSessions returns the live stdio sessions (test introspection).
func (b *stdioBridge) snapshotSessions() []*stdioSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*stdioSession, 0, len(b.sessions))
	for _, s := range b.sessions {
		out = append(out, s)
	}
	return out
}

// mcpechoBuilder compiles the testdata server once per process.
type mcpechoBuilder struct {
	once   sync.Once
	binary string
	err    error
}

var echoBuilder mcpechoBuilder

// buildEchoServer compiles the testdata mcpecho stdio server once per test
// run and returns its path.
func buildEchoServer(t *testing.T) string {
	t.Helper()
	echoBuilder.once.Do(func() {
		dir, err := os.MkdirTemp("", "mcpecho-build")
		if err != nil {
			echoBuilder.err = err
			return
		}
		echoBuilder.binary = dir + "/mcpecho"
		build := exec.Command("go", "build", "-o", echoBuilder.binary, "./testdata/mcpecho")
		if out, err := build.CombinedOutput(); err != nil {
			echoBuilder.err = err
			t.Logf("mcpecho build output: %s", out)
		}
	})
	require.NoError(t, echoBuilder.err)
	return echoBuilder.binary
}
