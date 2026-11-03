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

package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newCodeInterpreterTestServer builds an execd test server that answers /ping
// and streams /command results. runtimeOK controls whether the runtime process
// check command succeeds; commandCalls counts /command requests.
func newCodeInterpreterTestServer(t *testing.T, runtimeOK func(attempt int32) bool, commandCalls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ping":
			w.WriteHeader(http.StatusOK)
		case "/command":
			var req RunCommandRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			attempt := atomic.AddInt32(commandCalls, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			if runtimeOK(attempt) {
				fmt.Fprint(w, "data: {\"type\":\"execution_complete\",\"execution_time\":1}\n\n")
			} else {
				fmt.Fprint(w, "data: {\"type\":\"error\",\"ename\":\"CommandExecError\",\"evalue\":\"1\"}\n\n")
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestCodeInterpreter(t *testing.T, url string) *CodeInterpreter {
	t.Helper()
	sb := &Sandbox{
		id:    "ci-test-sandbox",
		execd: NewExecdClient(url, "tok"),
	}
	return &CodeInterpreter{Sandbox: sb}
}

func TestCodeInterpreter_IsHealthy(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return true }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	require.True(t, ci.IsHealthy(context.Background()), "IsHealthy should pass when the runtime process is up")
	require.Equal(t, int32(1), atomic.LoadInt32(&calls), "runtime check should run once")
}

func TestCodeInterpreter_IsHealthy_FailsWhenRuntimeProcessMissing(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return false }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	require.True(t, !ci.IsHealthy(context.Background()), "IsHealthy should fail when the runtime process is missing")
	require.True(t, atomic.LoadInt32(&calls) >= 1, "runtime check should have been attempted")
}

func TestCodeInterpreter_WaitRuntimeReady_PollsUntilRuntimeUp(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return attempt >= 3 }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	err := ci.waitRuntimeReady(context.Background(), 5*time.Second, 5*time.Millisecond)
	require.NoError(t, err, "waitRuntimeReady")
	require.Equal(t, int32(3), atomic.LoadInt32(&calls), "runtime check should pass on the third attempt")
}

func TestCodeInterpreter_WaitRuntimeReady_TimesOut(t *testing.T) {
	var calls int32
	srv := newCodeInterpreterTestServer(t, func(attempt int32) bool { return false }, &calls)
	ci := newTestCodeInterpreter(t, srv.URL)

	err := ci.waitRuntimeReady(context.Background(), 50*time.Millisecond, 5*time.Millisecond)
	require.Error(t, err, "waitRuntimeReady should time out")
	var readyErr *SandboxReadyTimeoutError
	require.ErrorAs(t, err, &readyErr, "timeout error type")
	assert.Contains(t, readyErr.Error(), "jupyter", "timeout message should mention the runtime")
}

func TestCreateCodeInterpreter_NegativeHealthCheckIntervalUsesDefault(t *testing.T) {
	var commandCalls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			jsonResponse(w, http.StatusCreated, SandboxInfo{
				ID:        "sbx-ci-interval",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sbx-ci-interval":
			jsonResponse(w, http.StatusOK, SandboxInfo{
				ID:        "sbx-ci-interval",
				Status:    SandboxStatus{State: StateRunning},
				CreatedAt: time.Now().UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/sbx-ci-interval/endpoints/44772":
			jsonResponse(w, http.StatusOK, Endpoint{Endpoint: srv.URL})
		case r.URL.Path == "/ping":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/command":
			atomic.AddInt32(&commandCalls, 1)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"error\",\"ename\":\"CommandExecError\",\"evalue\":\"1\"}\n\n")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	// CreateSandbox treats a non-positive interval as the default; the runtime
	// readiness poll must do the same instead of busy-looping on /command.
	_, err := CreateCodeInterpreter(context.Background(), ConnectionConfig{
		Domain:         srv.URL,
		DisableMetrics: true,
	}, CodeInterpreterCreateOptions{
		ReadyTimeout:        500 * time.Millisecond,
		HealthCheckInterval: -time.Millisecond,
	})
	var readyErr *SandboxReadyTimeoutError
	require.ErrorAs(t, err, &readyErr, "runtime never comes up")
	calls := atomic.LoadInt32(&commandCalls)
	require.True(t, calls <= 5, fmt.Sprintf("runtime check ran %d times in 500ms, want the default 200ms spacing", calls))
}

// newFragmentServer replays one execd stream on path: an init event, one event
// per fragment on the given stream ("stdout" or "stderr"), and a completion.
func newFragmentServer(t *testing.T, path, stream string, fragments []string) *httptest.Server {
	t.Helper()
	events := []map[string]any{{"type": "init", "text": "exec-fragments", "timestamp": 0}}
	for i, text := range fragments {
		events = append(events, map[string]any{"type": stream, "text": text, "timestamp": i + 1})
	}
	events = append(events, map[string]any{"type": "execution_complete", "timestamp": len(fragments) + 1})

	var body strings.Builder
	for _, event := range events {
		data, err := json.Marshal(event)
		require.NoError(t, err, "marshal event")
		fmt.Fprintf(&body, "data: %s\n\n", data)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body.String())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func outputTexts(messages []OutputMessage) []string {
	var texts []string
	for _, m := range messages {
		texts = append(texts, m.Text)
	}
	return texts
}

func TestCodeInterpreter_Execute_TextJoinsStreamFragments(t *testing.T) {
	tests := []struct {
		name      string
		fragments []string
		want      string
	}{
		{name: "partial line", fragments: []string{"Loading", "...done\n"}, want: "Loading...done\n"},
		{name: "progress dots", fragments: []string{".", ".", "."}, want: "..."},
		{name: "terminated lines", fragments: []string{"first\n", "second\n"}, want: "first\nsecond\n"},
		{name: "blank line", fragments: []string{"first\n", "\n", "last\n"}, want: "first\n\nlast\n"},
		{name: "only blank line", fragments: []string{"\n"}, want: "\n"},
		{name: "carriage returns", fragments: []string{"first\r", "last\r"}, want: "first\rlast\r"},
		{name: "CRLF lines", fragments: []string{"first\r\n", "last\r\n"}, want: "first\r\nlast\r\n"},
		{name: "split CRLF", fragments: []string{"first\r", "\n", "last\r", "\n"}, want: "first\r\nlast\r\n"},
		{name: "unterminated final fragment", fragments: []string{"first\n", "last"}, want: "first\nlast"},
		{name: "empty fragments", fragments: []string{"", "Loading", "", "...done\n", ""}, want: "Loading...done\n"},
		{name: "no output", want: ""},
	}
	for _, stream := range []string{"stdout", "stderr"} {
		for _, tc := range tests {
			t.Run(stream+"/"+tc.name, func(t *testing.T) {
				srv := newFragmentServer(t, "/code", stream, tc.fragments)
				ci := newTestCodeInterpreter(t, srv.URL)

				exec, err := ci.Execute(context.Background(), "python", "print('synthetic stream')", nil)
				require.NoError(t, err, "Execute")
				require.Equal(t, "exec-fragments", exec.ID)
				require.NotNil(t, exec.Complete, "execution_complete")

				// The fragments stay intact; Text() reports stdout only.
				if stream == "stdout" {
					require.Equal(t, tc.fragments, outputTexts(exec.Stdout), "stdout fragments")
					if got := exec.Text(); got != tc.want {
						t.Errorf("Text() = %q, want %q", got, tc.want)
					}
					return
				}
				require.Equal(t, tc.fragments, outputTexts(exec.Stderr), "stderr fragments")
				if got := exec.Text(); got != "" {
					t.Errorf("Text() = %q, want no stderr text", got)
				}
			})
		}
	}
}

func TestCodeInterpreter_ExecuteEntryPoints_TextJoinsStreamFragments(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(ci *CodeInterpreter) (*Execution, error)
	}{
		{name: "Execute", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.Execute(ctx, "python", "print('synthetic stream')", nil)
		}},
		{name: "ExecuteInContext", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.ExecuteInContext(ctx, "ctx-1", "python", "print('synthetic stream')", nil)
		}},
		{name: "Sandbox.ExecuteCode", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.Sandbox.ExecuteCode(ctx, RunCodeRequest{
				Context: &CodeContext{Language: "python"},
				Code:    "print('synthetic stream')",
			}, nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFragmentServer(t, "/code", "stdout", []string{"Loading", "...done\n"})
			exec, err := tc.run(newTestCodeInterpreter(t, srv.URL))
			require.NoError(t, err, tc.name)
			if got, want := exec.Text(), "Loading...done\n"; got != want {
				t.Errorf("Text() = %q, want %q", got, want)
			}
		})
	}
}

func TestCodeInterpreter_ShellModesKeepLineSeparators(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		run  func(ci *CodeInterpreter) (*Execution, error)
	}{
		{name: "Execute empty language", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.Execute(ctx, "", "echo a; echo b", nil)
		}},
		{name: "ExecuteInContext empty language", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.ExecuteInContext(ctx, "ctx-1", "", "echo a; echo b", nil)
		}},
		{name: "ExecuteCode no context", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.Sandbox.ExecuteCode(ctx, RunCodeRequest{Code: "echo a; echo b"}, nil)
		}},
		{name: "ExecuteCode empty context", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.Sandbox.ExecuteCode(ctx, RunCodeRequest{
				Context: &CodeContext{},
				Code:    "echo a; echo b",
			}, nil)
		}},
		{name: "Execute command language", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.Execute(ctx, "command", "echo a; echo b", nil)
		}},
		{name: "Execute background-command language", run: func(ci *CodeInterpreter) (*Execution, error) {
			return ci.Execute(ctx, "background-command", "echo a; echo b", nil)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFragmentServer(t, "/code", "stdout", []string{"a", "b"})
			exec, err := tc.run(newTestCodeInterpreter(t, srv.URL))
			require.NoError(t, err, tc.name)
			if got, want := exec.Text(), "a\nb"; got != want {
				t.Errorf("Text() = %q, want %q", got, want)
			}
		})
	}
}

// Command output keeps joining events as lines: only a code execution is
// treated as a stream of fragments.
func TestExecution_Text_CommandPathsKeepLineSeparators(t *testing.T) {
	ctx := context.Background()
	paths := []struct {
		name string
		path string
		run  func(sb *Sandbox) (*Execution, error)
	}{
		{name: "RunCommand", path: "/command", run: func(sb *Sandbox) (*Execution, error) {
			return sb.RunCommand(ctx, "echo", nil)
		}},
		{name: "RunInSession", path: "/session/sess-1/run", run: func(sb *Sandbox) (*Execution, error) {
			return sb.RunInSession(ctx, "sess-1", RunInSessionRequest{Command: "echo"}, nil)
		}},
		{name: "IsolationSession.Run", path: "/v1/isolated/session/iso-1/run", run: func(sb *Sandbox) (*Execution, error) {
			session := &IsolationSession{sandbox: sb, info: &IsolatedSessionInfo{SessionID: "iso-1"}}
			return session.Run(ctx, IsolatedRunRequest{Code: "echo"}, nil)
		}},
	}
	tests := []struct {
		name      string
		fragments []string
		want      string
	}{
		{name: "stripped lines", fragments: []string{"a", "b"}, want: "a\nb"},
		{name: "partial line", fragments: []string{"Loading", "...done\n"}, want: "Loading\n...done\n"},
		{name: "terminated lines", fragments: []string{"a\n", "b\n"}, want: "a\nb\n"},
	}
	for _, p := range paths {
		for _, tc := range tests {
			t.Run(p.name+"/"+tc.name, func(t *testing.T) {
				srv := newFragmentServer(t, p.path, "stdout", tc.fragments)
				sb := &Sandbox{id: "command-test-sandbox", execd: NewExecdClient(srv.URL, "tok")}
				exec, err := p.run(sb)
				require.NoError(t, err, p.name)
				if got := exec.Text(); got != tc.want {
					t.Errorf("Text() = %q, want %q", got, tc.want)
				}
			})
		}
	}
}
